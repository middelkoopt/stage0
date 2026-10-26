# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**Two images, two boards:** `stage0 install k52 --image rocky-10.2-rpi`
(Pi 5) and `stage0 install k400 --image alma-10.1-rpi` (Pi 400) both end
with the node answering ping and SSH at its stage0 lease (`10.0.3.100`,
`10.0.3.101`), site keys, its hostname. The Rocky image doesn't boot a
Pi 400 (firmware can't read its boot partition; see
[`docs/images.md`](docs/images.md)). Both nodes are on.

## Open decisions
- **WW1** How stage0 gets `CAP_NET_RAW` for router advertisements: a
  setcap'd copy installed once, a small helper, or skip RAs. Your `sudo`
  "ask" rule prompts for every setcap after a rebuild.
- **WW2** What VLAN `install` should move a node's port to afterwards
  (stage 0.5); the head also needs to stay reachable from stage0.

## Next session
1. [`ROADMAP.md`](ROADMAP.md#next) item 2: the agent reports to stage0
   over HTTP, so `install` works without a serial console.
2. Then item 4 (Raspberry Pi OS or Ubuntu for Pi: MBR, cloud-init), or
   milestone 2 (the QEMU test bed).
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
