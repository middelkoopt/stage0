# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-27)
**Pis:** three images install and come up with SSH on the Pi 5 (k52) and
the Pi 400 (k400) (Rocky 10.2: Pi 5 only), over IPv4 or, with
`install --ipv6`, IPv6 alone; bootloaders at 2026-09. k52 runs Raspberry
Pi OS (`10.0.3.100`), k400 Alma (`10.0.3.101`), both on. **VMs:**
`stage0 install vm1 --image rocky-10.2-vm` installs Rocky's aarch64 cloud
image in a QEMU VM on the private test network (96 s;
[`docs/testbed.md`](docs/testbed.md)).

## Open decisions
(none)

## Next session
1. IPv6: `install --ipv6` works end to end on k52 (Rocky, Raspberry Pi
   OS) and k400 (Alma) ([`docs/netboot.md`](docs/netboot.md#ipv6-netboot-on-a-pi-5));
   run from `/usr/local/bin/stage0` (`tools/install-capped.sh` after a
   rebuild); both nodes have `USE_IPV6=1` in their EEPROM. Not done: VMs
   over IPv6 (`--ipv6` refuses; `stage0tap0` would need `fd00:10:9::1`).
2. Milestone 4 (network modes) or 5 (UEFI/x86_64)
   ([`ROADMAP.md`](ROADMAP.md#milestones-order-tentative)).
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
