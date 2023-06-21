package bundles

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
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
	gtr, err := cnf.api.GetTableRowsOrder(fio.GetTableRowsOrderRequest{
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
		log.Infof("%s is expired, skipping", a.DbResponse.Address+"@"+a.DbResponse.Domain)
		a.Expired = true
		return false, expiredErr{}
	}
	log.Debugf("%s has %d bundled transactions remaining", a.DbResponse.Address+"@"+a.DbResponse.Domain, result[0].Remaining)
	if result[0].Remaining < uint32(cnf.minBundleTx) {
		needsBundle = true
		log.Infof("Address, %s, below min tx threshold! Will refresh", a.DbResponse.Address+"@"+a.DbResponse.Domain)
	} else if result[0].Remaining < uint32(2*cnf.minBundleTx) {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshDuration + randMinutes(10)) // wait > 60 minutes and < 70 minutes
		log.Debugf("Address, %s, above min tx threshold. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, a.Refreshed.Format(time.RFC3339))
	} else if result[0].Remaining < uint32(4*cnf.minBundleTx) {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshDuration*4 + randMinutes(30)) // wait > 4 hours and < 4.5 hours
		log.Debugf("Address, %s, above min tx threshold. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, a.Refreshed.Format(time.RFC3339))
	} else {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshDuration*12 + randMinutes(60)) // wait > 12 hours and < 13 hours
		log.Debugf("Address, %s, above min tx threshold. Will recheck at %s", a.DbResponse.Address+"@"+a.DbResponse.Domain, a.Refreshed.Format(time.RFC3339))
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
			log.Warn(a + " is not a valid formatted address")
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
		log.Debug("Processing addresses stored in state...")
		expired := make([]string, 0)
		for k, v := range ac.Addresses {
			if v.Stale() {
				if u, e := v.CheckRemaining(); e != nil {
					switch e.(type) {
					case expiredErr:
						expired = append(expired, k)
						continue
					}
					// if not found, it has been purged from state.
					if e.Error() == "address not found" {
						expired = append(expired, k)
						log.Debug(k + " - purging non-existent (on chain) address")
						continue
					}
					log.Warnf("An error occured checking addresses for remaining bundled transactions. Error: %s", e.Error())
					v.Refreshed = time.Now().UTC().Add(randMinutes(10))
					log.Debug(fmt.Sprintf("Will retry at %s", v.Refreshed.Format(time.RFC3339)))
					continue
				} else if u {
					addBundle <- v.DbResponse

					// Set an initial cooldown period to slow attacks
					// Note that it is possible the addBundles tx failed. In this case, the tx will be attempted again
					// in cnf.refreshDuration + randMinutes(10). If not, the Refreshed attribute will be updated again
					var cooldown time.Duration = cnf.refreshDuration + randMinutes(10)
					v.Refreshed = time.Now().UTC().Add(cooldown)
					log.Infof("Address bundled tx refreshed. Setting initial cooldown period of %s", cooldown.String())
				}
			}
		}
		ac.mux.RUnlock()
		for i := range expired {
			ac.delete(expired[i])
		}
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
