#!/bin/zsh
# Stages the Container Manager project for a Synology NAS (x86-64):
#   packaging/synology/build.sh OUTDIR
# OUTDIR gets Dockerfile, compose.yaml and the linux/amd64 tidymail binary.
# Copy mail.d next to them separately.
set -eu
out=${1:?usage: $0 OUTDIR}
here=${0:A:h}
repo=${here:h:h}
mkdir -p "$out"
(cd "$repo" && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o "$out/tidymail" ./cmd/tidymail)
cp "$here/Dockerfile" "$here/compose.yaml" "$out/"
print "staged $out"
