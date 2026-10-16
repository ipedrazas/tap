#!/usr/bin/env bash
# The apps layer of the platform: the services in deploy/platform/apps and
# deploy/factory/apps, whose images git names without a digest.
#
#   apps.sh images        the images in the apps layer
#   apps.sh inputs IMAGE  the paths IMAGE's build reads
#   apps.sh tag IMAGE     IMAGE's input tag: the first 12 characters of a
#                         hash of its inputs' contents at HEAD (the git
#                         objects of every file), with -dirty if the working
#                         tree changes or adds to them. Exits 1 for images
#                         outside the apps layer.
#   apps.sh refs          every image at its input tag, as name@digest
#   apps.sh render        the apps layer with those digests, on stdout
#
# Images are pushed under their input tag (task images:push), so a commit that
# doesn't change an image's inputs reuses the image already in the registry,
# and the same sources always render the same digest: a revert gets back the
# exact image that ran before, without a rebuild.
set -euo pipefail
cd "$(dirname "$0")/.."

REGISTRY=${REGISTRY:-registry.hiddenfield.dev}
IMAGES="console hooks egress-proxy factory factory-publisher"

# inputs IMAGE: the paths its build reads. Go images: the module's packages
# the binaries import (embedded files included), go.mod, go.sum and the
# image's own directory.
inputs() {
  local pkgs extra=""
  case $1 in
    console | hooks | egress-proxy | factory-publisher) pkgs=./cmd/$1 ;;
    factory) pkgs="./cmd/tapctl ./cmd/runner" extra="factory/package.json factory/package-lock.json factory/src" ;;
    *) return 1 ;;
  esac
  # shellcheck disable=SC2086
  go list -deps -f '{{if .Module}}{{if .Module.Main}}{{.Dir}}{{end}}{{end}}' $pkgs | sed "s#^$PWD/##" | sort -u ||
    { echo "$1: go list failed; can't tell its inputs" >&2; return 1; }
  # shellcheck disable=SC2086
  echo go.mod go.sum "images/$1" $extra | tr ' ' '\n'
}

tag() {
  local paths hash
  case " $IMAGES " in *" $1 "*) ;; *) echo "$1 is not in the apps layer" >&2; return 1 ;; esac
  paths=$(inputs "$1") || return 1
  # Path, mode and blob id of every input file at HEAD, hashed: the contents,
  # not the history, so it works in a shallow clone too.
  # shellcheck disable=SC2086
  hash=$(git ls-tree -r HEAD -- $paths | git hash-object --stdin)
  # shellcheck disable=SC2086
  if git diff --quiet HEAD -- $paths && [ -z "$(git ls-files --others --exclude-standard -- $paths)" ]; then
    echo "${hash:0:12}"
  else
    echo "${hash:0:12}-dirty"
  fi
}

refs() {
  local image t digest
  for image in $IMAGES; do
    t=$(tag "$image")
    case $t in *-dirty) echo "$image: inputs have uncommitted changes; using ${t%-dirty}" >&2 ;; esac
    digest=$(crane digest "$REGISTRY/tap/$image:${t%-dirty}" 2>/dev/null) ||
      { echo "$REGISTRY/tap/$image:${t%-dirty} not found; run: task images:push IMAGES=$image" >&2; return 1; }
    echo "$REGISTRY/tap/$image@$digest"
  done
}

# render: an overlay over both apps directories that sets each image's digest.
# platform.yaml lives outside the kustomization (configMapGenerator), hence
# LoadRestrictionsNone.
render() {
  local out overlay ref
  out=$(refs)
  # Inside the repo: kustomize refuses absolute resource paths.
  mkdir -p .task
  overlay=$(mktemp -d .task/apps.XXXXXX)
  trap 'rm -rf "$overlay"' RETURN
  {
    echo "resources: [../../deploy/platform/apps, ../../deploy/factory/apps]"
    echo "images:"
    for ref in $out; do echo "  - { name: ${ref%@*}, digest: ${ref#*@} }"; done
  } > "$overlay/kustomization.yaml"
  kubectl kustomize --load-restrictor LoadRestrictionsNone "$overlay"
}

case ${1:-} in
  images) echo "$IMAGES" ;;
  inputs) [ $# -eq 2 ] || { echo "usage: $0 inputs IMAGE" >&2; exit 2; }; inputs "$2" || { echo "$2 is not in the apps layer" >&2; exit 1; } ;;
  tag) [ $# -eq 2 ] || { echo "usage: $0 tag IMAGE" >&2; exit 2; }; tag "$2" ;;
  refs) refs ;;
  render) render ;;
  *) echo "usage: $0 images | inputs IMAGE | tag IMAGE | refs | render" >&2; exit 2 ;;
esac
