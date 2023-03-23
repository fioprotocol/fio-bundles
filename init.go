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

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ssm"
)

// config holds all of our connection information.
type config struct {
	addressTicker time.Duration // address monitor timeout - address tx check
	bundlesTicker time.Duration // process monitor timeout - process check
	dbTicker      time.Duration // db wallet/account query timeout - data check
	txTicker      time.Duration // transaction timeout - add bundle check
	txFinalTicker time.Duration // transaction finalization timeout - tx cleanup

	refreshDuration  time.Duration // minimum time to wait before re-checking if an address needs more bundles.
	coolDownDuration time.Duration // minimum time to wait to after an address is bundled

	awsRegion string // Parameter store region
	uat       bool   // UAT Registration DB/Parameters

	nodeosApiUrl string
	wif          string
	dbUrl        string // expects account:password@hostname/databasename
	stateFile    string
	permission   string // expect account@permission

	api         *fio.API
	acc         *fio.Account
	pg          *pgxpool.Pool
	state       *AddressCache
	minBundleTx uint
	persistTx   bool
	verbose     bool
}

// cnf is a _package-level_ variable holding a config
var cnf config

// erCache is a _package-level_ map holding the event results (add bundle transaction metadata)
var erCache map[string]*EventResult

// matcher is a compiled global regexp
var matcher = regexp.MustCompile(`^\w+@\w+$`)

var env_param1 string
var env_param2 string

// init parses flags or checks environment variables, it updates the package-level 'cnf' struct.
func init() {
	// Set static parameters
	cnf.addressTicker = 30 * time.Second
	cnf.bundlesTicker = 5 * time.Minute
	cnf.dbTicker = time.Minute
	cnf.txTicker = time.Minute
	cnf.txFinalTicker = time.Minute

	cnf.refreshDuration = 15 * time.Minute
	cnf.coolDownDuration = time.Hour

	cnf.persistTx = false

	log.SetFlags(log.LstdFlags | log.Lshortfile)

	flag.StringVar(&cnf.dbUrl, "d", os.Getenv("DB"), "Required: db connection string. Alternates; ENV ('DB')/AWS Parameter Store")
	flag.StringVar(&cnf.nodeosApiUrl, "u", os.Getenv("NODEOS_API_URL"), "Required: nodeos API url. Alternates; ENV ('NODEOS_API_URL')/AWS Parameter Store")
	flag.StringVar(&cnf.wif, "k", os.Getenv("WIF"), "Required: private key WIF. Alternates; ENV ('WIF')/AWS Parameter Store")
	flag.StringVar(&cnf.stateFile, "f", "state.dat", "Optional: state cache filename.")
	flag.StringVar(&cnf.permission, "p", "", "Optional: permission to use to authorize transaction ex: actor@active.")
	flag.UintVar(&cnf.minBundleTx, "b", 5, "Optional: minimum bundled transaction threshold at which an address is renewed.")
	flag.BoolVar(&cnf.uat, "t", false, "Optional: use the UAT registration db/parameters")
	flag.BoolVar(&cnf.verbose, "v", false, "verbose logging")
	flag.Parse()

	emptyFatal := func(s, m string) {
		if s == "" {
			flag.PrintDefaults()
			fmt.Println("")
			log.Fatal(m)
		}
	}

	cnf.awsRegion = "us-east-1"
	env_param1 = "node"
	env_param2 = "production"
	if cnf.uat {
		cnf.awsRegion = "us-west-2"
		env_param1 = "uat"
		env_param2 = "staging"
	}

	sess, err := session.NewSessionWithOptions(session.Options{
		Config:            aws.Config{Region: aws.String(cnf.awsRegion)},
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		panic(err)
	}
	ssmsvc := ssm.New(sess)

	if cnf.dbUrl == "" {
		var dbUrlParam = fmt.Sprintf("/registration/%s/DATABASE_URL", env_param1)
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String(dbUrlParam),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.dbUrl = *param.Parameter.Value
	}
	if cnf.nodeosApiUrl == "" {
		// /bundles-services-fio-bundles/staging/NODEOS_API_URL
		var apiUrlParam = fmt.Sprintf("/bundles-services-fio-bundles/%s/NODEOS_API_URL", env_param2)
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String(apiUrlParam),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.nodeosApiUrl = *param.Parameter.Value
	}
	if cnf.wif == "" {
		var wifParam = fmt.Sprintf("/registration/%s/WALLET_PRIVATE_KEY", env_param1)
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String(wifParam),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.wif = *param.Parameter.Value
	}

	// Validate required settings
	if cnf.dbUrl == "" {
		emptyFatal(cnf.dbUrl, "No database connection information specified, provide '-d' or set 'DB'")
	}
	if cnf.nodeosApiUrl == "" {
		emptyFatal(cnf.nodeosApiUrl, "No nodeos API URL specified, provide '-u' or set 'NODEOS_API_URL'")
	}
	if cnf.wif == "" {
		emptyFatal(cnf.wif, "No private key present, provide '-k' or set 'WIF'")
	}

	// Validate optional settings
	if cnf.permission != "" {
		if b := matcher.Match([]byte(cnf.permission)); !b {
			log.Fatal("permission should be in format actor@permission, got:", cnf.permission)
		}
	}

	// Validate state file exists (either default or file explicitly set on command line)
	emptyFatal(cnf.stateFile, "state cache file cannot be empty")

	logInfo(fmt.Sprintf("DB URL:          %s", cnf.dbUrl))
	logInfo(fmt.Sprintf("NODEOS API URL:  %s", cnf.nodeosApiUrl))
	logInfo(fmt.Sprintf("WIF:             %s", cnf.wif))
	logInfo(fmt.Sprintf("PERM:            %s", cnf.permission))
	logInfo(fmt.Sprintf("Data File:       %s", cnf.stateFile))
	logInfo(fmt.Sprintf("Min Bundle Tx:   %d", cnf.minBundleTx))
	logInfo(fmt.Sprintf("Production Env:  %t", !cnf.uat))
	logInfo(fmt.Sprintf("Verbose Logging: %t", cnf.verbose))

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
	logInfo(fmt.Sprintf("NewWifConnect Account: Actor = %s", cnf.acc.Actor))

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
