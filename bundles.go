package bundles

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"github.com/fioprotocol/fio-go/eos"
)

// Run is the main entrypoint. It will setup channels, launch routines for handling the event queue, and has
// a watchdog that will exit if any goroutine appears stalled.
func Run() {
	log.Println("Starting the fio-bundles application...")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// watchdog channels
	addrsAlive, txAlive, dbAlive := make(chan time.Time), make(chan time.Time), make(chan time.Time)
	addrsLast, txLast, dbLast := time.Now().UTC(), time.Now().UTC(), time.Now().UTC()

	// address channels from db, and needs more bundles
	foundAddr, addBundles := make(chan *AddressResponse), make(chan *AddressResponse)

	go watchDb(ctx, foundAddr, dbAlive)
	go cnf.state.watch(ctx, foundAddr, addBundles, addrsAlive)
	go handleTx(ctx, addBundles, txAlive)
	if cnf.persistTx {
		logInfo("Enabling Transaction metadata persistence")
		go watchFinal(ctx)
	}

	watchdog := time.NewTicker(cnf.bundlesTicker)
	for {
		select {
		case <-ctx.Done():
			return

		// watchdog updates
		case addrsLast = <-addrsAlive:
		case txLast = <-txAlive:
		case dbLast = <-dbAlive:

		case <-watchdog.C:
			expired := time.Now().UTC().Add(-cnf.bundlesTicker)
			if expired.After(addrsLast) || expired.After(txLast) || expired.After(dbLast) {
				log.Println("ERROR: watchdog detected stalled goroutine")
				cancel()
				save(syscall.SIGQUIT)
			}
		}
	}
}

// save is called on any type of exit, and attempts to persist the cache to disk
func save(sig os.Signal) {
	if cnf.pg != nil {
		cnf.pg.Close()
	}
	log.Println("received", sig, "attempting to save state")
	f, err := os.OpenFile(cnf.stateFile, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		_ = f.Close()
		log.Fatal(err)
	}
	b, err := json.Marshal(cnf.state)
	if err != nil {
		_ = f.Close()
		log.Fatal(err)
	}
	_, _ = f.Write(b)
	_ = f.Close()
	log.Fatal("exiting")
}

// logInfo prints detailed log information.
func logInfo(v interface{}) {
	if !cnf.verbose {
		return
	}
	switch v.(type) {
	case eos.APIError:
		_ = log.Output(2, fmt.Sprintf("%s: %+v", v.(eos.APIError).Error(), v.(eos.APIError).ErrorStruct))
	case error:
		_ = log.Output(2, v.(error).Error())
	case string:
		_ = log.Output(2, v.(string))
	default:
		_ = log.Output(2, fmt.Sprintf("%+v", v))
	}
}
