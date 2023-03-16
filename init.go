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
	apiUrl     string
	wif        string
	dbUrl      string // expects account:password@hostname/databasename
	stateFile  string
	permission string // expect account@permission

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

// init parses flags or checks environment variables, it updates the package-level 'cnf' struct.
func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	flag.StringVar(&cnf.apiUrl, "u", os.Getenv("NODEOS_API_URL"), "Required: nodeos API url. Alternates; ENV ('NODEOS_API_URL')/AWS Parameter Store")
	flag.StringVar(&cnf.dbUrl, "d", os.Getenv("DB"), "Required: db connection string. Alternates; ENV ('DB')/AWS Parameter Store")
	flag.StringVar(&cnf.wif, "k", os.Getenv("WIF"), "Required: private key WIF. Alternates; ENV ('WIF')/AWS Parameter Store")
	flag.StringVar(&cnf.stateFile, "f", "state.dat", "Optional: state cache filename.")
	flag.StringVar(&cnf.permission, "p", "", "Optional: permission to use to authorize transaction ex: actor@active.")
	flag.UintVar(&cnf.minBundleTx, "b", 5, "Optional: minimum bundled transaction threshold at which an address is renewed.")
	flag.BoolVar(&cnf.persistTx, "t", false, "Optional: persist transaction data to the database.")
	flag.BoolVar(&cnf.verbose, "v", false, "verbose logging")
	flag.Parse()

	emptyFatal := func(s, m string) {
		if s == "" {
			flag.PrintDefaults()
			fmt.Println("")
			log.Fatal(m)
		}
	}

	sess, err := session.NewSessionWithOptions(session.Options{
		Config:            aws.Config{Region: aws.String("us-west-2")},
		SharedConfigState: session.SharedConfigEnable,
	})
	if err != nil {
		panic(err)
	}
	ssmsvc := ssm.New(sess)

	if cnf.apiUrl == "" {
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String("/bundles-services-fio-bundles/staging/NODEOS_API_URL"),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.apiUrl = *param.Parameter.Value
	}
	if cnf.dbUrl == "" {
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String("/registration/uat/DATABASE_URL"),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.dbUrl = *param.Parameter.Value
	}
	if cnf.wif == "" {
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String("/registration/uat/WALLET_PRIVATE_KEY"),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.wif = *param.Parameter.Value
	}

	// Validate required settings
	if cnf.apiUrl == "" {
		emptyFatal(cnf.apiUrl, "No nodeos API URL specified, provide '-u' or set 'NODEOS_API_URL'")
	}
	if cnf.dbUrl == "" {
		emptyFatal(cnf.dbUrl, "No database connection information specified, provide '-d' or set 'DB'")
	}
	if cnf.wif == "" {
		emptyFatal(cnf.wif, "No private key present, provide '-k' or set 'WIF'")
	}

	// Validate optional settings
	if cnf.permission != "" {
		if b := matcher.Match([]byte(cnf.permission)); !b {
			log.Fatal("permission should be in format account@permission, got:", cnf.permission)
		}
	}

	// Validate state file exists (either default or file explicitly set on command line)
	emptyFatal(cnf.stateFile, "state cache file cannot be empty")

	logInfo(fmt.Sprintf("NODEOS API URL:  %s", cnf.apiUrl))
	logInfo(fmt.Sprintf("DB URL:          %s", cnf.dbUrl))
	logInfo(fmt.Sprintf("WIF:             %s", cnf.wif))
	logInfo(fmt.Sprintf("PERM:            %s", cnf.permission))
	logInfo(fmt.Sprintf("Data File:       %s", cnf.stateFile))
	logInfo(fmt.Sprintf("Min Bundle Tx:   %d", cnf.minBundleTx))
	logInfo(fmt.Sprintf("Tx Persistence:  %t", cnf.persistTx))
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
	cnf.acc, cnf.api, _, e = fio.NewWifConnect(cnf.wif, cnf.apiUrl)
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
