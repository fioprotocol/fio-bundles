#!/bin/bash

# Clean up previous build artifacts
rm -rf dist api_list.txt bundles
mkdir -p dist

# Copy resource file to project root. Note, the command-line arg, nodeosApiUrls, will override this resource
echo
echo "Choose the appropriate resource from the menu below"
echo
PS3="Select the Fio API nodeos resource: "
options=("Production" "UAT" "Custom" "None")
select option in "${options[@]}" Quit
do
    case $REPLY in
        1) echo;echo "#$REPLY) Production API resource";cp resources/prod_apis.txt api_list.txt;break;;
        2) echo;echo "#$REPLY) UAT API resource";cp resources/uat_apis.txt api_list.txt;break;;
        3) echo;echo -n "#$REPLY) A custom API nodes resource selected. Create the file, 'api_list.txt' in the root directory. "; read -p "Press any key to proceed..."; break;;
        4) echo;echo -n "#$REPLY) No API resource selected (will be provided via command-line arg). ";read -p "Press any key to proceed...";touch api_list.txt;break;;
        $((${#options[@]}+1))) echo;echo "Note: An API resource is required! Exiting build";exit 1;;
        *) echo;echo "Unknown choice entered: $REPLY. Please try again.";echo
    esac
done

# Build the application
echo
echo "Build the FIO Bundles applications..."
go build -trimpath -o dist/bundles cmd/bundles/main.go
if [[ $? -eq 0 ]]; then
  echo
  echo "FIO Bundles application may be found in the dist folder"
else
  echo
  echo "An error occurred building the FIO Bundles application! Review console output and rebuild."
fi
echo

# Clean up resource artifact
rm -f api_list.txt