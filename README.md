# fio-bundles

This tool handles the automatic refresh of bundles. It queries the registration site database for all fio handles that are marked as "auto-refresh" and then refreshes them if they are low.

It requires access to the registration site database and a FIO API node.

A local cache is used to limit the number of database queries. There is some rudimentary rate limiting on refreshes for an address, requiring a random wait of between 1 and 2 hours.

A watchdog is built in, so if the process stalls it will detect this and restart itself.

Logs are added to the registration database upon completion of a refresh. The UI is not designed for this, but the logs can be viewed when viewing the details for the pub key associated to the address.

## Configuration

This is intended to be run in a docker container but may be run from the command line, for debugging/test purposes. The parameters below are required and, by default, will be pulled from the AWS parameter store. Parameters may be passed in on the command line as well. In this case, command line parameters take precedence.

* `DB` - the database connection string. Expected format is `postgres://user:password@host:port/database`
* `NODEOS_API_URL` - the URL of the FIO API node to use
* `WIF` - the private key to use for signing transactions
* `PERM` - the permission to use for signing transactions, e.g. `fio.address@active` this option is only needed if the account is using a delegated permission.

The `PERM` parameter may specified on the command line, pulled from the registration database or be set to the account associated to the WIF for delegated permission use cases.

In addition other configuration parameters, i.e., timers, address refresh/cool down durations, etc. are set directly in the config during initialization.

Configuration items of note are;
* The DB is queried for new wallets, and new addresses for each wallet (max of 500/wallet) every 30 minutes
* Address processing occurs every 5 minutes and once an individual address is processed, it is reprocessed based on its remaining bundled transactions;
    if < 10 the address is refreshed with new bundled transactions
    if < 20 the address is checked again 1 hour later
    if < 40 the address is checked 4 hours later
    if > 40 the address is checked 12 hours later

## Building

This is a standard go project, so can be built with `go build` or `go install`. The `Dockerfile` is provided for convenience and will provide a small container with the binary.

### Usage

```
$ bundles -h
  -b uint
    	Optional: minimum bundled transaction threshold at which an address is renewed. (default 5)
  -d string
    	Required: db connection string. Alternate ENV: DB
  -f string
    	Optional: state cache filename. Alternate ENV: FILE (default "state.dat")
  -k string
    	Required: private key WIF. Alternate ENV: WIF
  -p string
    	Optional: permission ex: actor@active. Alternate ENV: PERM
  -t  Optional: persist of transaction metadata to the registration db.
  -u string
    	Required: nodeos API url. Alternate ENV: NODEOS_URL
  -v	Optional: verbose logging.
```

See run-bundles.sh for examples of running the fio-bundles application.