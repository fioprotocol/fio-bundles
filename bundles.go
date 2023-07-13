package bundles

import (
	"context"
	"encoding/json"
	"os"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fioprotocol/fio-go"
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
		log.Info("Enabling Transaction metadata persistence")
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
			expired := time.Now().UTC().Add(-(72 * cnf.bundlesTicker))
			if expired.After(addrsLast) || expired.After(dbLast) || expired.After(txLast) {
				log.Errorf("Watchdog detected expired goroutine; Addr: %v, DB: %v, Tx: %v",
					expired.After(addrsLast), expired.After(dbLast), expired.After(txLast))
				cancel()
				save(syscall.SIGQUIT)
			}

			stalled := time.Now().UTC().Add(-cnf.bundlesTicker)
			addrStalled := stalled.After(addrsLast)
			dbStalled := stalled.After(dbLast)
			txStalled := stalled.After(txLast)
			if addrStalled || dbStalled || txStalled {
				log.Errorf("Watchdog detected stalled goroutine; Addr: %v, DB: %v, Tx: %v",
					addrStalled, dbStalled, txStalled)
				if addrStalled {
					log.Errorf("Watchdog detected Address goroutine is stalled. Likely reason is a connectivity issue with API Node(s). Investigate and restart if neccessary")
				}
				if dbStalled {
					log.Errorf("Watchdog detected DB goroutine is stalled. Likely reason is a connectivity issue with DB. Investigate and restart if neccessary")
				}
				if txStalled {
					log.Errorf("Watchdog detected Tx goroutine is stalled. Likely reason is a connectivity issue with API Node(s). Investigate and restart if neccessary")
				}
			}
		}
	}
}

// save is called on any type of exit, and attempts to persist the cache to disk
func save(sig os.Signal) {
	log.Println("Received", sig, "attempting to shut down gracefully...")
	log.Println("Disconnecting from DB...")
	if cnf.pg != nil {
		cnf.pg.Close()
		log.Println("Disconnected from DB.")
	}

	log.Println("Saving state...")
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
	log.Println("State saved.")
	log.Fatal("exiting")
}

// ApiSelector returns one of the apis using a simple round-robin algorithm
func ApiSelector() *fio.API {
	api := cnf.apis[roundRobinIndex]
	if log.IsLevelEnabled(log.TraceLevel) {
		log.Tracef("Selected API URL: %s, at Index: %d", api.BaseURL, roundRobinIndex)
	}
	roundRobinIndex++

	// it means that we reached the end of servers
	// and we need to reset the counter and start
	// from the beginning
	if roundRobinIndex >= len(cnf.apis) {
		roundRobinIndex = 0
	}
	return api
}

// logIt prints detailed error log information.
// Note: no way in logrus, slog, etc to skip frame in callstack
func logIt(v interface{}) {
	switch v.(type) {
	case eos.APIError:
		log.Errorf("%s: %+v", v.(eos.APIError).Error(), v.(eos.APIError).ErrorStruct)
	case error:
		log.Error(v.(error).Error())
	case string:
		log.Error(v.(string))
	default:
		log.Errorf("%+v", v)
	}
}

func maskLeft(s string) string {
	rs := []rune(s)
	if len(s) <= 8 {
		for i := range rs[:] {
			rs[i] = '*'
		}
		return string(rs)
	}

	for i := range rs[:len(rs)-8] {
		rs[i] = '*'
	}
	return string(rs)
}
