#!/bin/bash

echo -e "Pattern is set to $PATTERN"

if [ "$PATTERN" == "susehello" ] || [ "$PATTERN" == "sall" ]
then

# Configure the usehello service variables.
read -d '' snsconfig <<EOF
{
  "url": "my.company.com.services.usehello2",
  "version": "1.0.0",
  "organization": "e2edev@somecomp.com",
  "attributes": [
    {
      "type": "UserInputAttributes",
      "label": "User input variables",
      "publishable": false,
      "host_only": false,
      "mappings": {
        "MY_VAR1": "e2edev"
      }
    }
  ]
}
EOF

echo -e "\n\n[D] usehello service config payload: $snsconfig"

echo "Registering usehello service config on node"

ERR=$(echo "$snsconfig" | curl -sS -X POST -H "Content-Type: application/json" --data @- "$ANAX_API/service/config" | jq -r '.error')
if [ "$ERR" != "null" ]; then
  echo -e "error occured: $ERR"
  exit 2
fi

# Configure the hello service variables.
read -d '' snsconfig <<EOF
{
  "url": "my.company.com.services.hello2",
  "version": "1.0.0",
  "organization": "e2edev@somecomp.com",
  "attributes": [
    {
      "type": "UserInputAttributes",
      "label": "User input variables",
      "publishable": false,
      "host_only": false,
      "mappings": {
        "MY_S_VAR1": "e2edev"
      }
    }
  ]
}
EOF

echo -e "\n\n[D] hello service config payload: $snsconfig"

echo "Registering hello service config on node"

ERR=$(echo "$snsconfig" | curl -sS -X POST -H "Content-Type: application/json" --data @- "$ANAX_API/service/config" | jq -r '.error')
if [ "$ERR" != "null" ]; then
  echo -e "error occured: $ERR"
  exit 2
fi

# Configure the cpu service variables.
read -d '' snsconfig <<EOF
{
  "url": "my.company.com.services.cpu2",
  "version": "1.0.0",
  "organization": "e2edev@somecomp.com",
  "attributes": [
    {
      "type": "UserInputAttributes",
      "label": "User input variables",
      "publishable": false,
      "host_only": false,
      "mappings": {
        "MY_CPU_VAR": "e2edev"
      }
    }
  ]
}
EOF

echo -e "\n\n[D] cpu service config payload: $snsconfig"

echo "Registering cpu service config on node"

ERR=$(echo "$snsconfig" | curl -sS -X POST -H "Content-Type: application/json" --data @- "$ANAX_API/service/config" | jq -r '.error')
if [ "$ERR" != "null" ]; then
  echo -e "error occured: $ERR"
  exit 2
fi

fi