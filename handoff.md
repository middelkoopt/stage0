# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**`./stage0 install k52 --image rocky-10.2-rpi` installs Rocky 10.2 on k52
(Pi 5) in 4 minutes and leaves it reachable as `ssh root@…` with the
site's key and hostname `k52`** ([`docs/install.md`](docs/install.md)). Its
only address is IPv6 link-local (`fe80::5662:976b:2c19:7e38%eth0.3` today);
lab key: `~/.cache/stage0/ssh/id_ed25519`. k52 is on. Hostname and key
work is uncommitted.

## Open decisions
(none)

## Next session
1. The rest of first boot ([`ROADMAP.md`](ROADMAP.md#next) item 1): an
   address on `piwulf` (seed `root/` with a NetworkManager connection; the
   seed's `boot/`/`root/` split, GG2) and growing the root filesystem.
2. Try the same install on k400 (Pi 400, port 8, now on piwulf): the
   firmware should pick `kernel8.img`.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
