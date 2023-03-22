# fio-bundles

This tool handles the automatic refresh of bundles. It queries the registration site database for all fio handles that are marked as "auto-refresh" and then refreshes them if they are low.

It requires access to the registration site database and a FIO API node.

A local cache is used to limit the number of database queries. There is some rudimentary rate limiting on refreshes for an address, requiring a random wait of between 1 and 2 hours.

A watchdog is built in, so if the process stalls it will detect this and restart itself.

Logs are added to the registration database upon completion of a refresh. The UI is not designed for this, but the logs can be viewed when viewing the details for the pub key associated to the address.

## Configuration

This is intended to be run as a container, e.g. in TestNet or MainNet, but may be run from the command line, for debugging/test purposes. The parameters below are required and will be pulled from the AWS parameter store. These parameters may be passed in on the commnad line and will take precedence.

* `NODEOS_API_URL` - the URL of the FIO API node to use
* `DB` - the database connection string. Expected format is `postgres://user:password@host:port/database`
* `WIF` - the private key to use for signing transactions

The parameter PERM may specified on the command line, pulled from the registration database or be set to the account associated to the WIF along with the default permission of "active"for delegated permission use cases.

* `PERM` - the permission to use for signing transactions, e.g. `fio.address@active` this option is only needed if the account is using a delegated permission.

In addition several other parameters, i.e., timers, transaction persistance, add bundle transaction refresh/cool down durations, etc. that are set directly on the config during initialization.

## Building

This is a standard go project, so can be built with `go build` or `go install`. The `Dockerfile` is provided for convenience and will provide a small container with the binary.

### Usage

```
$ bundles -h
  -d string
    	Required: db connection string. Alternate ENV: DB
  -f string
    	Optional: state cache filename. Alternate ENV: FILE (default "state.dat")
  -k string
    	Required: private key WIF. Alternate ENV: WIF
  -p string
    	Optional: permission ex: actor@active. Alternate ENV: PERM
  -u string
    	Required: nodeos API url. Alternate ENV: NODEOS_URL
  -v	verbose logging
```