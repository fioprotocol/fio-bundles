package bundles

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/fioprotocol/fio-go"
	"github.com/jackc/pgx/v4/pgxpool"
)

// config holds all of our connection information.
type config struct {
	url        string
	wif        string
	dbUrl      string // expects account:password@hostname/databasename
	permission string
	stateFile  string

	api     *fio.API
	acc     *fio.Account
	pg      *pgxpool.Pool
	state   *AddressCache
	verbose bool
}

// cnf is a _package-level_ variable holding a config
var cnf config

var erCache map[string]*EventResult

// init parses flags or checks environment variables, it updates the package-level 'cnf' struct.
func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	flag.StringVar(&cnf.url, "u", os.Getenv("NODEOS_URL"), "Required: nodeos API url. Alternate ENV: NODEOS_URL")
	flag.StringVar(&cnf.wif, "k", os.Getenv("WIF"), "Required: private key WIF. Alternate ENV: WIF")
	flag.StringVar(&cnf.permission, "p", os.Getenv("PERM"), "Optional: permission ex: actor@active. Alternate ENV: PERM")
	flag.StringVar(&cnf.stateFile, "f", "state.dat", "Optional: state cache filename. Alternate ENV: FILE")
	flag.StringVar(&cnf.dbUrl, "d", os.Getenv("DB"), "Required: db connection string. Alternate ENV: DB")
	flag.BoolVar(&cnf.verbose, "v", false, "verbose logging")
	flag.Parse()

	emptyFatal := func(s, m string) {
		if s == "" {
			flag.PrintDefaults()
			fmt.Println("")
			log.Fatal(m)
		}
	}

	if cnf.url == "" {
		emptyFatal(cnf.url, "No url specified, provide '-u' or set 'NODEOS_URL")
	}
	if cnf.wif == "" {
		emptyFatal(cnf.wif, "No private key present, provide '-k' or set 'WIF'")
	}
	if cnf.dbUrl == "" {
		emptyFatal(cnf.dbUrl, "No database connection information provided")
	}
	if cnf.permission == "" {
		a, e := fio.NewAccountFromWif(cnf.wif)
		if e != nil {
			log.Fatal(e)
		}
		cnf.permission = string(a.Actor) + "@active"
	}

	emptyFatal(cnf.stateFile, "state cache file cannot be empty")
	if b, _ := regexp.Match(`^\w+@\w+$`, []byte(cnf.permission)); !b {
		log.Fatal("permission should be in format account@permission, got:", cnf.permission)
	}

	var e error

	// connect to database
	timeout, cxl := context.WithTimeout(context.Background(), 10*time.Second)
	defer cxl()
	cnf.pg, e = pgxpool.Connect(timeout, cnf.dbUrl)
	if e != nil {
		log.Fatal(e)
	}
	e = cnf.pg.Ping(timeout)
	if e != nil {
		log.Fatal(e)
	}

	// connect to API
	cnf.acc, cnf.api, _, e = fio.NewWifConnect(cnf.wif, cnf.url)
	if e != nil {
		log.Fatal(e)
	}

	// transactions to monitor for finalization
	erCache = make(map[string]*EventResult)

	// load the cached address list
	func() {
		badState := func(e error) {
			log.Println("could not open state file, starting with empty state.", e.Error())
			cnf.state = &AddressCache{Addresses: make(map[string]*Address, 0)}
		}
		f, err := os.Open(cnf.stateFile)
		if err != nil {
			badState(err)
			return
		}
		defer f.Close()
		body, err := io.ReadAll(f)
		if err != nil {
			badState(err)
			return
		}
		cnf.state = &AddressCache{}
		err = json.Unmarshal(body, cnf.state)
		if err != nil {
			badState(err)
		}
	}()

	// save the address cache on quit
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigs
		save(sig)
	}()
}
