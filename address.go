package bundles

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fioprotocol/fio-go"
)

// Address holds information used in the address cache
type Address struct {
	Hash       string
	Refreshed  time.Time
	Expired    bool
	DbResponse *AddressResponse
}

// NewAddress creates an Address
func NewAddress(dbr *AddressResponse) *Address {
	return &Address{
		Hash:       fio.AddressHash(dbr.Address + "@" + dbr.Domain),
		DbResponse: dbr,
	}
}

// Stale determines if an address should be checked for remaining bundles
func (a Address) Stale() bool {
	if a.Expired {
		return false
	}
	return time.Now().UTC().After(a.Refreshed)
}

// minAddrResult is a trimmed down response from the GetTableRowsOrder query
type minAddrResult struct {
	Id         uint64 `json:"id"` // used as a canary for a bad query result, should never be 0
	Expiration int64  `json:"expiration"`
	Remaining  uint32 `json:"bundleeligiblecountdown"`
}

// expiredErr is used to differentiate between a tx error and if an address has expired.
// Deprecated: soon addresses will not expire and this will be removed
type expiredErr struct{}

func (e expiredErr) Error() string {
	return ""
}

// CheckRemaining queries the chain for remaining bundles.
func (a *Address) CheckRemaining() (needsBundle bool, err error) {
	gtr, err := ApiSelector().GetTableRowsOrder(fio.GetTableRowsOrderRequest{
		Code:       "fio.address",
		Scope:      "fio.address",
		Table:      "fionames",
		LowerBound: a.Hash,
		UpperBound: a.Hash,
		Limit:      1,
		KeyType:    "i128",
		Index:      "5",
		JSON:       true,
		Reverse:    false,
	})
	if err != nil {
		return false, err
	}
	result := make([]minAddrResult, 0)
	err = json.Unmarshal(gtr.Rows, &result)
	if err != nil {
		return false, err
	}
	if len(result) == 0 || result[0].Id == 0 {
		return false, errors.New("address not found")
	}
	if result[0].Expiration != 0 && result[0].Expiration < time.Now().UTC().Unix() {
		a.Expired = true
		return false, expiredErr{}
	}
	var txCount = result[0].Remaining
	if txCount <= uint32(cnf.minBundleTx) {
		needsBundle = true
		log.Infof("Address, %s, txCount = %d, is at or below min tx threshold! Refreshing transactions...", a.DbResponse.Address+"@"+a.DbResponse.Domain, txCount)
	} else if txCount <= uint32(cnf.minBundleTx2x) {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshTimeout + randMinutes(10)) // wait > 30 minutes and < 40 minutes
		log.Debugf("Address, %s, tx count = %d. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, txCount, a.Refreshed.Format(time.RFC3339))
	} else if txCount <= uint32(cnf.minBundleTx4x) {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshTimeout*4 + randMinutes(30)) // wait > 2 hours and < 2.5 hours
		log.Debugf("Address, %s, tx count = %d. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, txCount, a.Refreshed.Format(time.RFC3339))
	} else if txCount <= uint32(cnf.minBundleTx8x) {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshTimeout*8 + randMinutes(60)) // wait > 4 hours and < 5 hours
		log.Debugf("Address, %s, tx count = %d. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, txCount, a.Refreshed.Format(time.RFC3339))
	} else {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshTimeout*16 + randMinutes(120)) // wait > 8 hours and < 10 hours
		log.Debugf("Address, %s, tx count = %d. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, txCount, a.Refreshed.Format(time.RFC3339))
	}
	return
}

// AddressCache holds the list of addresses discovered, allows tracking when to query for bundle depletion,
// and tracks the lowest height in the accounts table to query.
type AddressCache struct {
	mux          sync.RWMutex
	walletMux    sync.RWMutex
	MinDbAccount map[int]int // wallet_id and account_id
	Addresses    map[string]*Address
}

// add appends a new address into the cache
func (ac *AddressCache) add(addr *AddressResponse) {
	ac.mux.Lock()
	defer ac.mux.Unlock()
	a := addr.Address + "@" + addr.Domain
	if ac.Addresses[a] == nil {
		if !fio.Address(a).Valid() {
			log.Warn("Invalid address format: " + a)
			return
		}
		ac.Addresses[a] = NewAddress(addr)
		log.Debug("Added address to cache: " + a)
		return
	}
}

// delete removes an address from the cache
func (ac *AddressCache) delete(s string) {
	log.Debug("Deleting address from cache, address: " + s)
	ac.mux.Lock()
	defer ac.mux.Unlock()

	if ac.Addresses[s] == nil {
		log.Warn("Address is nil! Unable to delete")
		return
	}
	delete(ac.Addresses, s)
}

func randMinutes(max int) time.Duration {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return time.Duration(int(binary.LittleEndian.Uint32(b)>>1)%(max*60)) * time.Second
}

// watch loops over a ticker, and requests addresses be queried for depletion.
func (ac *AddressCache) watch(ctx context.Context, foundAddr, addBundle chan *AddressResponse, heartbeat chan time.Time) {
	tick := time.NewTicker(cnf.addressTicker)

	var busy bool
	checkAddresses := func() {
		if busy {
			log.Debug("Address watcher already running; returning")
			return
		}
		busy = true
		defer func() {
			busy = false
		}()
		ac.mux.RLock()

		// Debug/trace address logging setup
		var sb strings.Builder
		if log.IsLevelEnabled(log.TraceLevel) {
			_, _ = sb.WriteString("Addresses in timeout: ")
		}

		log.Infof("Checking new/stale addresses...")
		var i int
		end := time.Now().Add(cnf.addressTimeout)
		expired := make([]string, 0)
		for k, v := range ac.Addresses {
			// Check timeout every 16 iterations and break out of loop after timeout
			// Note: this prevents ongoing address processing to allow other goroutines to execute
			if i&0x0f == 0 {
				if time.Now().After(end) {
					// Call the schduler to allow another goroutine to run
					runtime.Gosched()
				}
			}
			var stale = v.Stale()
			if stale {
				if u, e := v.CheckRemaining(); e != nil {
					switch e.(type) {
					case expiredErr:
						expired = append(expired, k)
						log.Debugf("Address, %s, is expired, skipping", k)
						continue
					}
					// if not found, it has been purged from state.
					if e.Error() == "address not found" {
						expired = append(expired, k)
						log.Debugf("Address, %s, not found on-chain. Setting as expired, skipping", k)
						continue
					}
					v.Refreshed = time.Now().UTC().Add(randMinutes(10))
					log.Warnf("An error occured checking address, %s, for remaining bundled transactions. Error: %s", k, e.Error())
					log.Infof("Will retry address, %s, at %s", k, v.Refreshed.Format(time.RFC3339))
					continue
				} else if u {
					addBundle <- v.DbResponse

					// Set an initial cooldown period to slow attacks. The Refreshed attribute will be updated again
					// during address processing above. Note that it is possible the addBundles tx failed. In this
					// case, the tx will be attempted again after the timeout.
					var cooldown time.Duration = cnf.refreshTimeout + randMinutes(30)
					v.Refreshed = time.Now().UTC().Add(cooldown)
					log.Infof("Address, %s, bundled tx refreshed. Setting initial cooldown period of %s", k, cooldown.String())
				}
			}
		}

		if log.IsLevelEnabled(log.TraceLevel) {
			msg := sb.String()

			if len(msg) > 0 {
				msg = msg[:len(msg)-1]
			}
			log.Trace(msg)
		}

		ac.mux.RUnlock()
		for i := range expired {
			ac.delete(expired[i])
		}
		log.Infof("Total valid addresses found to date: %d", len(ac.Addresses))
		heartbeat <- time.Now().UTC()
	}

	for {
		select {
		case <-ctx.Done():
			log.Info("Address watcher exiting")
			return

		case <-tick.C:
			checkAddresses()

		case s := <-foundAddr:
			ac.add(s)
		}
	}
}
