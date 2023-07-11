#!/bin/bash

# Copy resource file to project root. Note, the command-line arg, nodeosApiUrls, will override this resource
rm -f api_list.txt
PS3="Select the Fio API nodeos resource: "
options=("MainNet" "TestNet" "Custom" "None")
select option in "${options[@]}" Quit
do
    case $REPLY in
        1) echo "#$REPLY) MainNet API resource";cp resources/mainnet_apis.txt api_list.txt;break;;
        2) echo "#$REPLY) TestNet API resource";cp resources/testnet_apis.txt api_list.txt;break;;
        3) echo -n "#$REPLY) A custom API nodes resource selected. Create the file, 'api_list.txt' in the root directory. "; read -p "Press any key to proceed..."; break;;
        4) echo -n "#$REPLY) No API resource selected (will be provided via command-line arg). ";read -p "Press any key to proceed...";touch api_list.txt;break;;
        $((${#options[@]}+1))) echo "Note: An API resource is required! Exiting build";exit 1;;
        *) echo "Unknown choice entered: $REPLY. Exiting build";exit 1;;
    esac
done

# Build the application
go build -trimpath -o bundles cmd/bundles/main.go
