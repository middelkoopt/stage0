#!/bin/sh
# Installs this repo's stage0 build (./stage0) as /usr/local/bin/stage0 with
# CAP_NET_RAW (router advertisements) and CAP_NET_ADMIN (neighbour entries
# for IPv6 netboot). It runs exactly these two
# sudo commands; .claude/settings.local.json allows this script without a
# prompt (the human's choice), while other sudo still asks.
set -eu
cd "$(dirname "$0")/.."
sudo install -m 0755 stage0 /usr/local/bin/stage0
sudo setcap cap_net_admin,cap_net_raw=ep /usr/local/bin/stage0
getcap /usr/local/bin/stage0
