#!/bin/sh
# Run the Go checks the way they are meant to run: vet, then the whole
# suite under the race detector. Some regression tests (the login-page
# deviceName read, the mDNS child reaping) only detect their bug under
# -race, so a plain `go test` passes them vacuously.
#
# Dev server only - never on a unit (see DEPLOYMENT.md). Needs cgo and a
# race-capable platform (linux/amd64 or linux/arm64).
set -e
cd "$(dirname "$0")/.."
go vet ./...
go test -race -count=1 "${@:-./...}"
