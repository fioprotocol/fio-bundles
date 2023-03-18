package bundles

import (
	"context"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/fioprotocol/fio-go"
	"github.com/fioprotocol/fio-go/eos"
)

const finalized uint32 = 240

var erCacheMux = sync.Mutex{}

func watchFinal(ctx context.Context) {

	var busy bool
	checkQueue := func() {
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
				/* Note: It's not necessary to update the database, the app will handle it on its own. */
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
			if response.BlockNum >= uint32(v.BlockNum)+finalized {
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

	tick := time.NewTicker(cnf.txFinalTicker)
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
	tick := time.NewTicker(cnf.txTicker)

	for {
		select {
		case <-ctx.Done():
			log.Println("transaction watcher exiting")

		case <-tick.C:
			cnf.api.RefreshFees()
			heartbeat <- time.Now().UTC()

		case s := <-addBundle:
			var actor = cnf.acc.Actor
			var permission = cnf.permission

			// If no permission specified then go to the db
			if permission == "" {
				var aa, ap string // authorization: actor, permission
				row := cnf.pg.QueryRow(ctx, "select actor, permission from account_profile where name like '%free%'")
				err := row.Scan(&aa, &ap)
				if err == nil {
					// Override actor with autorization actor
					actor = eos.AccountName(aa)
					permission = fmt.Sprintf("%s@%s", aa, ap)
					log.Println("Permission found in db, permission: ", permission)
				}
			}

			// Validate the permission format
			if permission != "" {
				if b := matcher.Match([]byte(permission)); !b {
					log.Println("permission is not in format actor@permission, got: ", permission)
					continue
				}
			}

			logInfo(fmt.Sprintf("Address to replenish: %s, Wallet Id: %d", s.Address+"@"+s.Domain, s.WalletId))
			logInfo(fmt.Sprintf("Account to use in tx: %s", actor))
			logInfo(fmt.Sprintf("Permission to use in tx: %s", permission))
			add, err := fio.NewAddBundlesWithPerm(fio.Address(s.Address+"@"+s.Domain), 1, actor, permission)
			if err != nil {
				log.Println(err)
				continue
			}

			// Get info about the chain
			gi, err := cnf.api.GetInfo()
			if err != nil {
				log.Println("could not refresh block height before tx", err)
			}

			// Set up event for this transaction
			event := &EventResult{
				Addr: s,
			}

			// Send transaction to chain
			result, err := cnf.api.SignPushActions(add)

			// Log and persist the transaction (if tx persistence is turned on)
			if err != nil {
				log.Printf("adding bundle for id %d failed: %s (%+v)", s.AccountId, err.Error(), err.(eos.APIError).ErrorStruct)
			} else {
				log.Printf("tx %s submitted for %+v", result.TransactionID, s)
				event.TrxId = result.TransactionID
				event.BlockNum = int(gi.HeadBlockNum) + 1
			}
			if cnf.persistTx {
				logTransaction(ctx, event, err != nil)
			}
		}
	}
}

func logTransaction(ctx context.Context, event *EventResult, txError bool) (err error) {
	err = event.createTrx(ctx)
	if err != nil {
		log.Println(err)
		return
	}

	if txError {
		err = event.logTrxResult(ctx, trxError, fmt.Sprintf("addbundles error: %v", err.(eos.APIError).ErrorStruct.Details))
	} else {
		err = event.logTrxResult(ctx, trxNew, "addbundles")
	}
	if err != nil {
		log.Println(err)
		return
	}

	err = event.updateLastTrx(ctx)
	if err != nil {
		log.Println(err)
		return
	}

	if txError {
		return
	}

	erCacheMux.Lock()
	erCache[event.TrxId] = event
	erCacheMux.Unlock()

	return nil
}
