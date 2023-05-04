package bundles

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

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
		logInfo(fmt.Sprintf("%s is expired, skipping", a.DbResponse.Address+"@"+a.DbResponse.Domain))
		a.Expired = true
		return false, expiredErr{}
	}
	logInfo(fmt.Sprintf("%s has %d bundled transactions remaining", a.DbResponse.Address+"@"+a.DbResponse.Domain, result[0].Remaining))
	if result[0].Remaining < uint32(cnf.minBundleTx) {
		needsBundle = true
		logInfo(fmt.Sprintf("%s needs bundled transactions", a.DbResponse.Address+"@"+a.DbResponse.Domain))
	} else {
		a.Refreshed = time.Now().UTC().Add(cnf.refreshDuration + randMinutes(4))
		logInfo(fmt.Sprintf("Will recheck at %s.", a.Refreshed.Format(time.UnixDate)))
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
			logInfo(a + " is not a valid formatted address")
			return
		}
		ac.Addresses[a] = NewAddress(addr)
		logInfo("added: " + a)
		return
	}
}

// delete removes an address from the cache
func (ac *AddressCache) delete(s string) {
	logInfo("not watching bundles for: " + s)
	ac.mux.Lock()
	defer ac.mux.Unlock()

	if ac.Addresses[s] == nil {
		logInfo("address was nil! not deleting")
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
			logInfo("not checking addresses, job already running")
			return
		}
		busy = true
		defer func() {
			busy = false
		}()
		ac.mux.RLock()
		logInfo("processing addresses stored in state")
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
						logInfo(k + " - purging non-existent address")
						continue
					}
					log.Println(e)
					v.Refreshed = time.Now().UTC().Add(cnf.refreshDuration + randMinutes(10))
					logInfo(fmt.Sprintf("Will retry at %s.", v.Refreshed.Format(time.UnixDate)))
					continue
				} else if u {
					addBundle <- v.DbResponse
					// min 1 hour before re-up to slow attacks
					var cooldown time.Duration = cnf.coolDownDuration + randMinutes(60)
					v.Refreshed = time.Now().UTC().Add(cooldown)
					logInfo(fmt.Sprintf("Address cooldown invoked. Will not check tx remaining for %s minutes.", cooldown.String()))
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
			log.Println("address cache watcher exiting")
			return

		case <-tick.C:
			checkAddresses()

		case s := <-foundAddr:
			ac.add(s)
		}
	}
}
