package bundles

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fioprotocol/fio-go"
	"github.com/jackc/pgx/v4/pgxpool"
)

// config holds all of our connection information.
type config struct {
	addressTicker time.Duration // address monitor timeout - address tx check
	bundlesTicker time.Duration // process monitor timeout - process check
	dbTicker      time.Duration // db wallet/account query timeout - data check
	txTicker      time.Duration // transaction timeout - add bundle check
	txFinalTicker time.Duration // transaction finalization timeout - tx cleanup

	refreshTimeout time.Duration // minimum time to wait before re-checking if an address needs more bundles.

	nodeosApiUrl string
	wif          string
	dbUrl        string // expects account:password@hostname/databasename
	stateFile    string
	permission   string // expect account@permission

	api   *fio.API
	acc   *fio.Account
	pg    *pgxpool.Pool
	state *AddressCache

	minBundleTx   uint
	minBundleTx2x uint
	minBundleTx4x uint
	minBundleTx8x uint
	persistTx     bool

	logLevel string // logrus logging level; must be one of Trace, Debug, Info, Warn, Error, Fatal, Panic.
}

// cnf is a _package-level_ variable holding a config
var cnf config

// erCache is a _package-level_ map holding the event results (add bundle transaction metadata)
var erCache map[string]*EventResult

// matcher is a compiled global regexp
var matcher = regexp.MustCompile(`^\w+@\w+$`)

// init parses flags or checks environment variables, it updates the package-level 'cnf' struct.
func init() {
	// Init static parameters
	cnf.addressTicker = 5 * time.Minute
	cnf.bundlesTicker = 15 * time.Minute
	cnf.dbTicker = 10 * time.Minute
	cnf.txTicker = time.Minute
	cnf.txFinalTicker = time.Minute

	cnf.refreshTimeout = 1 * time.Hour

	// Parse command-line args if any
	flag.StringVar(&cnf.dbUrl, "d", os.Getenv("DB"), "Required: db connection string. Alternates; ENV ('DB')")
	flag.StringVar(&cnf.nodeosApiUrl, "u", os.Getenv("NODEOS_API_URL"), "Required: nodeos API URL. Alternates; ENV ('NODEOS_API_URL')")
	flag.StringVar(&cnf.wif, "k", os.Getenv("WIF"), "Required: private key WIF. Alternates; ENV ('WIF')")
	flag.StringVar(&cnf.stateFile, "f", "state.dat", "Optional: state cache filename.")
	flag.StringVar(&cnf.permission, "p", "", "Optional: permission to use to authorize transaction ex: actor@active.")
	flag.UintVar(&cnf.minBundleTx, "b", 5, "Optional: minimum bundled transaction threshold at which an address is renewed. Default = 5.")
	flag.BoolVar(&cnf.persistTx, "t", false, "\nOptional: persist of transaction metadata to the registration db.")
	flag.StringVar(&cnf.logLevel, "l", "Info", "Optional: logrus log level. Default = 'Info'. Case-insensitive match else Default.")
	flag.Parse()

	// Init logger
	//log.SetFlags(log.LstdFlags | log.Lshortfile)

	log.SetReportCaller(false)

	// Json formatting
	//log.SetFormatter(&log.JSONFormatter{})
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	// log level
	switch strings.ToLower(cnf.logLevel) {
	case "panic":
		log.SetLevel(log.PanicLevel)
		break
	case "fatal":
		log.SetLevel(log.FatalLevel)
		break
	case "error":
		log.SetLevel(log.ErrorLevel)
		break
	case "warn":
		log.SetLevel(log.WarnLevel)
		break
	case "info":
		log.SetLevel(log.InfoLevel)
		break
	case "debug":
		log.SetLevel(log.DebugLevel)
		break
	case "trace":
		log.SetLevel(log.TraceLevel)
		break
	default:
		log.SetLevel(log.ErrorLevel)
	}

	emptyFatal := func(s, m string) {
		if s == "" {
			flag.PrintDefaults()
			fmt.Println("")
			log.Fatal(m)
		}
	}

	// Validate required settings
	if cnf.dbUrl == "" && cnf.nodeosApiUrl == "" && cnf.wif == "" {
		emptyFatal(cnf.dbUrl, "Required parameters not provided; DB URL, Nodeos API URL, WIF!")
	}
	if cnf.dbUrl == "" {
		emptyFatal(cnf.dbUrl, "No database connection information specified, provide '-d' or set 'DB'")
	}
	if cnf.nodeosApiUrl == "" {
		emptyFatal(cnf.nodeosApiUrl, "No nodeos API URL specified, provide '-u' or set 'NODEOS_API_URL'")
	}
	if cnf.wif == "" {
		emptyFatal(cnf.wif, "No private key present, provide '-k' or set 'WIF'")
	}

	// Validate state file exists (either default or file explicitly set on command line)
	emptyFatal(cnf.stateFile, "State cache file cannot be empty")

	// Validate optional settings
	if cnf.permission != "" {
		if b := matcher.Match([]byte(cnf.permission)); !b {
			log.Fatalf("Permission should be in format actor@permission, got: %s", cnf.permission)
		}
	}

	// Init min bundle settings for minBundleTxNx
	cnf.minBundleTx2x = 2 * cnf.minBundleTx
	cnf.minBundleTx4x = 4 * cnf.minBundleTx
	cnf.minBundleTx8x = 8 * cnf.minBundleTx

	log.Infof("DB URL:           %s", cnf.dbUrl)
	log.Infof("NODEOS API URL:   %s", cnf.nodeosApiUrl)
	// Mask out 'most' of the WIF
	log.Infof("WIF:              %s", maskLeft(cnf.wif))
	if cnf.permission != "" {
		log.Infof("PERM:             %s", cnf.permission)
	}
	log.Debugf("Data File:        %s", cnf.stateFile)
	log.Debugf("Min Bundle Tx:    %d", cnf.minBundleTx)
	log.Debugf("Persist Tx:       %t", cnf.persistTx)
	log.Debugf("Log Level:        %s", cnf.logLevel)

	var e error

	// connect to database
	timeout, cxl := context.WithTimeout(context.Background(), 10*time.Second)
	defer cxl()
	cnf.pg, e = pgxpool.Connect(timeout, cnf.dbUrl)
	if e != nil {
		cxl()
		log.Fatal(e)
	}
	e = cnf.pg.Ping(timeout)
	if e != nil {
		log.Fatal(e)
	}

	// connect to API
	cnf.acc, cnf.api, _, e = fio.NewWifConnect(cnf.wif, cnf.nodeosApiUrl)
	if e != nil {
		log.Fatal(e)
	}
	log.Info(fmt.Sprintf("NewWifConnect Account: Actor = %s", cnf.acc.Actor))

	// transactions to monitor for finalization
	erCache = make(map[string]*EventResult)

	// load the cached address list
	func() {
		badState := func(e error) {
			log.Warnf("Unable to process state file. %s", e.Error())
		}
		f, err := os.Open(cnf.stateFile)
		if err != nil {
			badState(err)
			log.Info("Starting with empty state.")
			cnf.state = &AddressCache{Addresses: make(map[string]*Address, 0)}
			return
		}
		body, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			badState(err)
			return
		}
		cnf.state = &AddressCache{}
		err = json.Unmarshal(body, cnf.state)
		if err != nil {
			badState(err)
		} else {
			log.Infof("Total addresses found in state: %d", len(cnf.state.Addresses))
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
