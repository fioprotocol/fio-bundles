# fio-bundles

This tool handles the automatic refresh of bundles. It queries the registration site database for wallets that are marked as "auto-refresh" as well as all fio handles, associated to those wallets, and refreshes active handles if their bundled transaction count is below the minimum.

The software requires access to the registration site database and a FIO API node.

A local cache is used to limit the number of database queries. There is some rudimentary rate limiting on refreshes for an address, requiring a random wait of between 1 and 2 hours.

Watchdogs are built in, so if any process stalls it will detect this and restart itself.

Transaction logging may be added to the registration database upon completion of a refresh. While the registration UI is not designed for this, but the logs can be viewed when viewing the details for the pub key associated to the address.

## Configuration

The following parameters are required and, by default, will be pulled from the AWS parameter store. Parameters may be passed in on the command line as well, and will take precedence. See the program help output for a complete list of parameters by executing `bundles -h` from the command line.

* `WIF` - the private key to use for signing transactions
* `DB` - the database connection string. Expected format is `postgres://user:password@host:port/database`

The following parameter is required and will be pulled from the registration db.
* `PERM` - the permission to use for signing transactions, e.g. `fio.address@active` this option is needed if the account is using a delegated permission. This parameter is currently pulled from the registration database and is associated to the WIF for delegated permission processing, however, it may also be provided on the command line or set in the environment.

The following parameter is required and will be read from a resource file within the application.
* `NODEOS_API_URLS` - FIO API URLs, indicating the Fio API nodes to use. A minimum of two API nodes are required. The expected value is `https://myfioapi.myfio.com,https://herfioapi.herfio.com,...`. These nodes are utilized in a round-robin fashion, as a means of load balancing across the given nodes.

In addition to these parameters, timers, address refresh/cool down durations, etc., are set directly in the config during initialization.

Items of note;
* The DB is queried for new wallets, and new addresses for each wallet (max of 500/wallet) every 2 minutes
* Address processing occurs every 45 seconds. Those addresses whose bundled transaction count is below the minimum limit will be refreshed with new bundled transactions. Each processed address will be reprocessed based on its remaining bundled transactions per the following rules;
  * if <= 5 the address will be refreshed with new bundled transactions
  * if 6-10 the address will be checked 30 minutes later (+ 1-10 minutes)
  * if 11-20 the address will be checked 2 hours later (+ 1-30 minutes)
  * if 21-40 the address will be checked 4 hours later (+ 1-60 minutes)
  * if > 40 the address will be checked 8 hours later (+ 1-120 minutes)
* For each refresh interval above a random delay of X minutes, denoted in the parenthesis above, is added to allow a load balance factor. The actual value of this random delay is also based on the nbr of remaining transactions, starting at 1-10 minutes and progressing to 1-120 minutes. For an address who tx count falls below the min threshhold and is refreshed, a cooldown period is added of 30-60 minutes. This cooldown period will be updated per the workflow above once the actual transaction count is determined.

## Building

This is a standard go project, and can be built with `go build` or `go install`. The `Dockerfile` is provided for convenience and is a minimal container with the binary. It must be enhanced to capture the appropriate command-line or env variables. The resource file, api_list.txt, will be built into the binary at build time. Copy or edit the api_list.txt file, including the nodeos API URLs that should be used for API interatction. See the resource directly for the current list of testnet and mainnet API URLs.

To build, perform the following steps from the project root;
* Create the file, api_list.txt, in the project root directory, and populate it with the list of Fio nodeos API URLs.
  * Refer to the file 'testnet_apis.txt', located in the resources directory, for a list of TestNet API URLs
  * Refer to the file 'mainnet_apis.txt', located in the resources directory, for a list of MainNet API URLs
* Execute the command `go build -trimpath -o bundles cmd/bundles/main.go`

### Usage

```
$ bundles -h
  -a string
    	Optional: 1 or more nodeos API URLs (comma delimited/no spaces).
  -b uint
    	Optional: minimum bundled transaction threshold at which an address is renewed. Default = 5. (default 5)
  -d string
    	Required: db connection string. Alternates; ENV ('DB')
  -f string
    	Optional: state cache filename. (default "state.dat")
  -k string
    	Required: private key WIF. Alternates; ENV ('WIF')
  -l string
    	Optional: logrus log level. Default = 'Info'. Case-insensitive match else Default. (default "Info")
  -p string
    	Optional: permission to use to authorize transaction ex: actor@active.
  -t	
    	Optional: persist of transaction metadata to the registration db.
  -v	verbose logging
```

See run-bundles.sh for examples of running the fio-bundles application.