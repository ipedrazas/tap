#!/bin/sh
#
# The operator's OpenBao token (policies tap-seeder, tap-signer,
# tap-tested-signer), kept outside the repo so losing .tap costs nothing:
# the macOS Keychain (service tap-openbao), or $XDG_CONFIG_HOME/tap/openbao.token
# (mode 0600) where there is no Keychain. TAP_BAO_TOKEN overrides both.
#
#   deploy/openbao/token.sh            print the token
#   deploy/openbao/token.sh --store    save the token read from stdin
#
# `task openbao:onboard` mints and stores it; rerun that to replace a lost one.
set -eu
service=tap-openbao
file="${XDG_CONFIG_HOME:-$HOME/.config}/tap/openbao.token"

if [ "${1:-}" = "--store" ]; then
  read -r token
  [ -n "$token" ] || { echo "no token on stdin" >&2; exit 1; }
  if command -v security >/dev/null 2>&1; then
    # security -i reads the command from stdin, so the token never shows in argv.
    printf 'add-generic-password -U -a %s -s %s -w %s\n' "$USER" "$service" "$token" | security -i >/dev/null
    echo "token stored in the Keychain (service $service)" >&2
  else
    mkdir -p "$(dirname "$file")"
    (umask 077 && printf '%s\n' "$token" > "$file")
    echo "token stored in $file" >&2
  fi
  exit 0
fi

if [ -n "${TAP_BAO_TOKEN:-}" ]; then
  printf '%s\n' "$TAP_BAO_TOKEN"
elif command -v security >/dev/null 2>&1 && security find-generic-password -a "$USER" -s "$service" -w 2>/dev/null; then
  :
elif [ -s "$file" ]; then
  cat "$file"
else
  echo "no OpenBao token; run: task openbao:onboard (or set TAP_BAO_TOKEN)" >&2
  exit 1
fi
