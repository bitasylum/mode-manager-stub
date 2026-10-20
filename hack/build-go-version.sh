#!/usr/bin/env bash
# Prints the Go version that the Dockerfile builds the image with, from its
# builder FROM line. make vulncheck and the CI workflows use it, so they check
# and test with the Go that ships rather than go.mod's minimum.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
# $BUILDPLATFORM is matched literally, as the Dockerfile has it.
# shellcheck disable=SC2016
version=$(sed -nE 's/^FROM (--platform=\$BUILDPLATFORM )?golang:([0-9]+\.[0-9]+\.[0-9]+)@sha256:[0-9a-f]{64} AS builder$/\2/p' Dockerfile)
if [[ -z $version || $version == *$'\n'* ]]; then
  echo "build-go-version: the Dockerfile needs one 'FROM [--platform=\$BUILDPLATFORM] golang:<x.y.z>@sha256:<digest> AS builder' line" >&2
  exit 1
fi
echo "$version"
