#!/bin/sh
# End-to-end check on the QEMU test bed: builds stage0 (static: it is also
# the agent) and installs a VM, by default vm1 with rocky-10.2-vm, printing
# install's JSON result; exits non-zero if the install fails. Needs KVM,
# qemu-system-aarch64, Debian's AAVMF firmware and the stage0tap0 tap
# (docs/testbed.md). Run from anywhere; works in the repo root.
set -eu
cd "$(dirname "$0")/.."
CGO_ENABLED=0 go build -o stage0 ./cmd/stage0
exec ./stage0 install "${1:-vm1}" --image "${2:-rocky-10.2-vm}" -o json
