package bundles

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/fioprotocol/fio-go/eos"
	"github.com/jackc/pgx/v4"
)

// trxStatus is used for logging transaction status in the database
type trxStatus string

const (
	trxNew     trxStatus = "pending"
	trxOk      trxStatus = "success"
	trxTimeout trxStatus = "expire"
	trxError   trxStatus = "error"
)

// AddressResponse holds the result of a database query for bundle-eligible addresses
type AddressResponse struct {
	AccountId int    `json:"account_id"`
	OwnerKey  string `json:"owner_key"`
	Address   string `json:"address"`
	Domain    string `json:"domain"`
}

// watchDb should be launched as a routine. It will push AddressResponse records to the cache as they are
// discovered.
func watchDb(ctx context.Context, foundAddr chan *AddressResponse, heartbeat chan time.Time) {
	var busy bool
	tick := time.NewTicker(time.Minute)

	doUpdate := func() {
		// prevent a mutex deadlock:
		if busy {
			logInfo("not querying database, job already running")
			return
		}
		busy = true
		defer func() {
			busy = false
		}()

		logInfo("updating wallets and addresses from registration database")
		err := updateWallets(ctx)
		if err != nil {
			log.Println("ERROR: could not update wallets from database, aborting address updates")
			return
		}
		for k, v := range cnf.state.MinDbAccount {
			found, e := getEligibleForBundle(ctx, k, v)
			if e != nil {
				log.Println("ERROR: could not query addresses for wallet id", k, e)
				return
			}
			for _, a := range found {
				foundAddr <- a
			}
		}
		heartbeat <- time.Now().UTC()
	}
	// run immediately at start:
	doUpdate()

	for {
		select {
		case <-ctx.Done():
			log.Println("db watcher exiting")
			return

		case <-tick.C:
			doUpdate()
		}
	}
}

// getEligibleForBundle queries the database table for eligible addresses
func getEligibleForBundle(ctx context.Context, walletId, minHeight int) ([]*AddressResponse, error) {
	rows, err := cnf.pg.Query(
		ctx,
		"select id, owner_key, address, domain from account"+
			"  where wallet_id=$1 and address is not null and id>$2"+
			"  order by id limit 500",
		walletId,
		minHeight,
	)
	if err != nil {
		logInfo(err)
		return nil, err
	}

	result := make([]*AddressResponse, 0)
	for rows.Next() {
		addr := &AddressResponse{}
		err = rows.Scan(&addr.AccountId, &addr.OwnerKey, &addr.Address, &addr.Domain)
		if err != nil {
			logInfo(err)
			return nil, err
		}
		result = append(result, addr)
	}
	if len(result) == 0 {
		return nil, nil
	}
	logInfo(fmt.Sprintf("found %d new addresses for wallet id %d", len(result), walletId))

	// update state with highest ID found to keep queries fast:
	cnf.state.walletMux.Lock()
	defer cnf.state.walletMux.Unlock()
	if cnf.state.MinDbAccount[walletId] < result[len(result)-1].AccountId {
		cnf.state.MinDbAccount[walletId] = result[len(result)-1].AccountId
	}

	return result, nil
}

// updateWallets ensures that all wallets with auto_bundles_add are in state
func updateWallets(ctx context.Context) error {
	if cnf.state.MinDbAccount == nil {
		cnf.state.MinDbAccount = make(map[int]int)
	}

	rows, err := cnf.pg.Query(ctx, "select id, name from wallet where auto_bundles_add = true")
	if err != nil {
		logInfo(err)
		return err
	}
	cnf.state.walletMux.Lock()
	defer cnf.state.walletMux.Unlock()
	for rows.Next() {
		var i int
		var s string
		err = rows.Scan(&i, &s)
		if err != nil {
			logInfo(err)
			return err
		}
		if cnf.state.MinDbAccount[i] == 0 {
			// use a 1 to forcibly create the map key:
			cnf.state.MinDbAccount[i] = 1
			logInfo(fmt.Sprintf("discovered new wallet id: %d", i))
		}
	}
	return nil
}

type EventResult struct {
	Id           int
	Status       trxStatus
	Addr         *AddressResponse
	TrxId        string
	Expiration   time.Time
	BlockNum     int
	TxErr        eos.APIError
	LastTrxEvent int
}

func beginTx(ctx context.Context) (timeout context.Context, cancel context.CancelFunc, tx pgx.Tx, err error) {
	timeout, cancel = context.WithTimeout(ctx, time.Minute)

	tx, err = cnf.pg.Begin(timeout)
	if err != nil {
		cancel()
		logInfo(err)
		return nil, nil, nil, err
	}
	return
}

// createTrx creates the initial entry in the blockchain_trx table.
func (er *EventResult) createTrx(ctx context.Context) (err error) {
	timeout, cancel, tx, err := beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	defer cancel()

	_, err = tx.Exec(
		timeout,
		"insert into blockchain_trx(type,trx_id,expiration,block_num,block_time,account_id) values ($1,$2,$3,$4,$5,$6)",
		"addbundles",
		er.TrxId,
		time.Now().Add(10*time.Minute).UTC(),
		er.BlockNum,
		time.Now().UTC(),
		er.Addr.AccountId,
	)
	if err != nil {
		logInfo(err)
		return err
	}

	err = tx.Commit(timeout)
	if err != nil {
		logInfo(err)
		return err
	}
	row := cnf.pg.QueryRow(ctx, "select id from blockchain_trx where trx_id=$1 order by id desc", er.TrxId)
	var i int
	err = row.Scan(&i)
	if err != nil {
		logInfo(err)
		return err
	}
	if i == 0 {
		err = errors.New("could not locate log row in blockchain_trx table")
		logInfo(err)
		return
	}
	er.Id = i

	return err
}

// updateLastTrx updates pointer to the last event for the logs.
func (er *EventResult) updateLastTrx(ctx context.Context) (err error) {
	if er.LastTrxEvent == 0 {
		err = errors.New("refusing to update last event with nonexistent transaction result")
		logInfo(err)
		return
	}

	timeout, cancel, tx, err := beginTx(ctx)
	if err != nil {
		return err
	}
	defer cancel()
	defer tx.Rollback(ctx)
	_, err = tx.Exec(
		timeout,
		"update blockchain_trx set last_trx_event=$1 where id=$2",
		er.LastTrxEvent,
		er.Id,
	)
	if err != nil {
		logInfo(fmt.Sprintf("update blockchain_trx set last_trx_event=%v where id=%v", er.LastTrxEvent, er.Id))
		logInfo(err)
		return
	}
	err = tx.Commit(timeout)
	if err != nil {
		logInfo(err)
	}

	return
}

// logTrxResult updates the blockchain_trx_event table with transaction details for the log in the UI.
// the eventId returned should be updated in the blockchain_trx table so that latest state is tracked.
func (er *EventResult) logTrxResult(ctx context.Context, status trxStatus, note string) (err error) {
	if er.Id == 0 {
		err = errors.New("cannot update non-existent blockchain_trx entry")
		logInfo(err)
		return
	}

	timeout, cancel, tx, err := beginTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	defer cancel()
	_, err = tx.Exec(
		timeout,
		"insert into blockchain_trx_event(created,trx_status,trx_status_notes,blockchain_trx_id) values ($1,$2,$3,$4)",
		time.Now().UTC(),
		status,
		note,
		er.Id,
	)
	if err != nil {
		logInfo(err)
		return
	}
	err = tx.Commit(timeout)
	if err != nil {
		logInfo(err)
		return
	}

	row := cnf.pg.QueryRow(ctx, "select id from blockchain_trx_event where blockchain_trx_id=$1 order by id desc", er.Id)
	var i int
	err = row.Scan(&i)
	if err != nil {
		logInfo(err)
		return err
	}
	if i == 0 {
		err = errors.New("could not locate log row in blockchain_trx table")
		logInfo(err)
		return
	}
	er.LastTrxEvent = i

	return
}

func (er *EventResult) isExpired(ctx context.Context) (expired bool, err error) {
	if er.Id == 0 {
		err = errors.New("cannot check nonexistent transaction for expiry")
		logInfo(err)
		return
	}
	rows := cnf.pg.QueryRow(ctx, "select expiration from blockchain_trx where trx_id=$1 order by expiration desc", er.TrxId)
	var ts time.Time
	err = rows.Scan(&ts)
	if err != nil {
		logInfo(err)
		return
	}

	if ts.Unix() == 0 {
		err = errors.New("could not determine timeout in blockchain_trx table")
		logInfo(err)
		return
	}

	return ts.Before(time.Now().UTC()), err

}
