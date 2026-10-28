# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-28)
**Installs:** four disk images on the Pis; the Rocky container from
podman's store, a (private) registry or a saved layout, partitioned by
the agent or Ignition; VMs by UEFI HTTP boot of a UKI. **First boot:**
stage0, Ignition or cloud-init, verified on Pis, containers and VMs
([`docs/firstboot.md`](docs/firstboot.md)). **Networks:** own, proxy
(beside another DHCP server and router) and none (firmware knows the
boot server), IPv4 and IPv6 ([`docs/network-modes.md`](docs/network-modes.md)).
**BMC:** `stage0 bmc` answers IPMI for the Pis: power by PoE, SOL by the
serial console. Nothing is pushed.

## Open decisions
(none)

## Next session
1. Choose the next arc ([`ROADMAP.md`](ROADMAP.md#milestones-order-tentative)):
   x86_64 and real BMCs (another lab), or what the roadmap lists later.
2. Nodes: k52 runs the Rocky container (from the private-registry test);
   k400 Raspberry Pi OS, EEPROM `USE_IPV6=0` (no static settings); vm1
   stopped. Nothing serves DHCP on pinet now. k53 is powered off; it may be powered
   and used as root, stopping (not disabling) its services. Its `sudo`
   needs a password stage0 doesn't have.
