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
	url        string
	wif        string
	dbUrl      string // expects account:password@hostname/databasename
	stateFile  string
	permission string // expect account@permission

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
	flag.StringVar(&cnf.dbUrl, "d", os.Getenv("DB"), "Required: db connection string. Alternate ENV: DB")
	flag.StringVar(&cnf.stateFile, "f", "state.dat", "Optional: state cache filename. Alternate ENV: FILE")
	flag.StringVar(&cnf.permission, "p", "", "Optional: permission ex: actor@active.")
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

	if cnf.url == "" {
		param, err := ssmsvc.GetParameter(&ssm.GetParameterInput{
			Name:           aws.String("/bundles-services-fio-bundles/staging/NODEOS_URL"),
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			panic(err)
		}
		cnf.url = *param.Parameter.Value
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

	// Validate required settings
	if cnf.url == "" {
		emptyFatal(cnf.url, "No url specified, provide '-u' or set 'NODEOS_URL")
	}
	if cnf.wif == "" {
		emptyFatal(cnf.wif, "No private key present, provide '-k' or set 'WIF'")
	}
	if cnf.dbUrl == "" {
		emptyFatal(cnf.dbUrl, "No database connection information provided")
	}
	if cnf.permission != "" {
		if b, _ := regexp.Match(`^\w+@\w+$`, []byte(cnf.permission)); !b {
			log.Fatal("permission should be in format account@permission, got:", cnf.permission)
		}
	}
	emptyFatal(cnf.stateFile, "state cache file cannot be empty")

	logInfo(fmt.Sprintf("API URL: %s", cnf.url))
	logInfo(fmt.Sprintf("DB URL:  %s", cnf.dbUrl))
	logInfo(fmt.Sprintf("WIF:     %s", cnf.wif))
	logInfo(fmt.Sprintf("PERM:    %s", cnf.permission))

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
	cnf.acc, cnf.api, _, e = fio.NewWifConnect(cnf.wif, cnf.url)
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
