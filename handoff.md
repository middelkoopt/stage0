# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**Three images install and come up with SSH:** Rocky 10.2 and Raspberry
Pi OS Lite trixie on k52 (Pi 5), AlmaLinux 10.1 on k400 (Pi 400), each at
its stage0 lease with the site's keys and its hostname; with or without a
serial console. The Rocky image doesn't boot a Pi 400 (see
[`docs/images.md`](docs/images.md)). k52 runs Raspberry Pi OS
(`10.0.3.100`), k400 Alma (`10.0.3.101`); both on.

## Open decisions
- **WW1** How stage0 gets `CAP_NET_RAW` for router advertisements: a
  setcap'd copy installed once, a small helper, or skip RAs. Your `sudo`
  "ask" rule prompts for every setcap after a rebuild.
- **WW2** What VLAN `install` should move a node's port to afterwards
  (stage 0.5); the head also needs to stay reachable from stage0.

## Next session
1. [`ROADMAP.md`](ROADMAP.md#next): Raspberry Pi OS or Ubuntu for Pi
   (MBR, cloud-init), or milestone 2 (the QEMU test bed). Installs no
   longer need a serial console (the agent reports over HTTP).
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
