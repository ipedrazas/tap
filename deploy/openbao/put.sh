#!/bin/sh
#
# Writes agent secrets to OpenBao (run through run.sh by task secrets:put and
# secrets:import). Stdin: the seeder token, then one "<mount> <path> <base64
# value>" line per secret. Values go through a memory-backed file, never argv.
set -eu
read -r BAO_TOKEN
export BAO_TOKEN
bao token renew >/dev/null
while read -r mount path b64; do
  [ -n "$path" ] || continue
  printf '%s' "$b64" | base64 -d > /tmp/value
  bao kv put -mount="$mount" "$path" value=@/tmp/value >/dev/null
  rm -f /tmp/value
  echo "put $mount/$path"
done
