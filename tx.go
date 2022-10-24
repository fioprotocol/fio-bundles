package bundles

import (
	"context"
	"encoding/hex"
	"fmt"
	"github.com/fioprotocol/fio-go"
	"github.com/fioprotocol/fio-go/eos"
	"log"
	"sync"
	"time"
)

const finalized uint32 = 240
var erCacheMux = sync.Mutex{}

func watchFinal(ctx context.Context) {

	var busy bool
	checkQueue := func(){
		if busy {
			return
		}
		busy = true
		erCacheMux.Lock()
		defer erCacheMux.Unlock()
		for k, v := range erCache {

			// log and cleanup expired transactions: these don't matter a lot, we'll get em' eventually
			expired, err := v.isExpired(ctx)
			if err != nil {
				log.Println(err)
				continue
			}
			if expired {
				logInfo("removing expired transaction from watch queue: " + v.TrxId)

				/* It's not necessary to update the database, the app will handle it on it's own.*/

				//err = v.logTrxResult(ctx, trxTimeout, "addbundles timed out")
				//if err != nil {
				//	log.Println(err)
				//	continue
				//}
				//err = v.updateLastTrx(ctx)
				//if err != nil {
				//	log.Println(err)
				//	continue
				//}
				delete(erCache, k)
				continue
			}

			// log confirmed transactions
			b, e := hex.DecodeString(v.TrxId)
			if e != nil {
				log.Println("could not decode trxid when checking finalization", e)
				continue
			}
			response, e := cnf.api.GetTransaction(b)
			if e != nil {
				log.Println(e)
				continue
			}
			if response.BlockNum >= uint32(v.BlockNum) + finalized {
				logInfo(fmt.Sprintf("marking tx %s as successful in database", v.TrxId))
				err = v.logTrxResult(ctx, trxOk, "addbundles transaction finalized")
				if err != nil {
					log.Println(err)
					continue
				}
				err = v.updateLastTrx(ctx)
				if err != nil {
					log.Println(err)
					continue
				}
				delete(erCache, k)
			}

		}
		busy = false
	}

	tick := time.NewTicker(time.Minute)
	for {
		select {
		case <-ctx.Done():
			return

		case <-tick.C:
			checkQueue()
		}
	}
}

func handleTx(ctx context.Context, addBundle chan *AddressResponse, heartbeat chan time.Time) {
	tick := time.NewTicker(time.Minute)

	for {
		select {
		case <-ctx.Done():
			log.Println("transaction watcher exiting")

		case <-tick.C:
			cnf.api.RefreshFees()
			heartbeat <- time.Now().UTC()

		case s := <-addBundle:
			add, err := fio.NewAddBundles(fio.Address(s.Address + "@" + s.Domain), 1, cnf.acc.Actor)
			if err != nil {
				log.Println(err)
				continue
			}

			event := &EventResult{
				Addr:         s,
			}
			gi, err := cnf.api.GetInfo()
			if err != nil {
				log.Println("could not refresh block height before tx", err)
			}

			result, err := cnf.api.SignPushActions(add)
			if err != nil {
				log.Printf("adding bundle for id %d failed: %s (%+v)", s.AccountId, err.Error(), err.(eos.APIError).ErrorStruct)
				err = event.createTrx(ctx)
				if err != nil {
					log.Println(err)
					continue
				}
				err = event.logTrxResult(ctx, trxError, fmt.Sprintf("addbundles error: %v", err.(eos.APIError).ErrorStruct.Details))
				if err != nil {
					log.Println(err)
					continue
				}
				err = event.updateLastTrx(ctx)
				if err != nil {
					log.Println(err)
				}
				continue
			}

			log.Printf("tx %s submitted for %+v", result.TransactionID, s)
			event.TrxId = result.TransactionID
			event.BlockNum = int(gi.HeadBlockNum) + 1

			// handle logging to postgres:
			err = event.createTrx(ctx)
			if err != nil {
				log.Println(err)
				continue
			}
			err = event.logTrxResult(ctx, trxNew, "addbundles")
			if err != nil {
				log.Println(err)
				continue
			}
			err = event.updateLastTrx(ctx)
			if err != nil {
				log.Println(err)
				continue
			}
			erCacheMux.Lock()
			erCache[event.TrxId] = event
			erCacheMux.Unlock()
		}
	}
}
