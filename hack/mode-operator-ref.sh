#!/usr/bin/env bash
# Prints the mode-operator commit that go.mod's version of the client library
# names, so the end-to-end tests run the operator the library was published
# with: a pseudo-version's commit, or the tag client/vX.Y.Z.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."
version=$(GOWORK=off go list -m -f '{{.Version}}' github.com/bitasylum/mode-operator/client)
if [[ $version =~ [.-][0-9]{14}-([0-9a-f]{12})$ ]]; then
  echo "${BASH_REMATCH[1]}"
elif [[ $version == v* ]]; then
  echo "client/$version"
else
  echo "mode-operator-ref: go.mod has no version of the client library" >&2
  exit 1
fi
