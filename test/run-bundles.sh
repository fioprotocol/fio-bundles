#!/bin/bash

# Cmd line arg Usage
#go run cmd/bundles/main.go -d  "postgres://registration:O40pWHb40mmdcNbNPUGtg2aIh3qSUX@reg-uat.cluster-cnxphxkusylu.us-west-2.rds.amazonaws.com:5432/registration" -k  "5JrWpJtuPyeXuFzhsAWYSyiT2gCZMR1d8ZdMPFhMkrkYE5NTvn7" -u "https://fiotestnet.blockpane.com" -v

# ENV parameter usage
#export DB="postgres://registration:O40pWHb40mmdcNbNPUGtg2aIh3qSUX@reg-uat.cluster-cnxphxkusylu.us-west-2.rds.amazonaws.com:5432/registration"
#export NODEOS_API_URL="https://fiotestnet.blockpane.com"
#export WIF="5JrWpJtuPyeXuFzhsAWYSyiT2gCZMR1d8ZdMPFhMkrkYE5NTvn7"
#go run cmd/bundles/main.go -v

# AWS Parameter Store Usage
go run cmd/bundles/main.go -t -v