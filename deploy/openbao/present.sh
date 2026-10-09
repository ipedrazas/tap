#!/bin/sh
#
# Reports which agent secrets exist in OpenBao, without reading them (run
# through run.sh by task agent:launch). Stdin: the seeder token, then one
# "<NAME> <mount> <path>" line per secret. Prints "present NAME" or
# "missing NAME"; the seeder may read metadata, never values.
set -eu
read -r BAO_TOKEN
export BAO_TOKEN
while read -r name mount path; do
  [ -n "$path" ] || continue
  if bao kv metadata get -mount="$mount" "$path" >/dev/null 2>&1; then
    echo "present $name"
  else
    echo "missing $name"
  fi
done
