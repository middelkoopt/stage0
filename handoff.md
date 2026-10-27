# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**Pis:** three images install and come up with SSH on the Pi 5 (k52) and
the Pi 400 (k400) (Rocky 10.2: Pi 5 only); both run Raspberry Pi OS
(`10.0.3.100`, `10.0.3.101`) and are on, their bootloaders updated to
2026-09 ([`docs/netboot.md`](docs/netboot.md#bootloader-eeprom-updates)). **VMs:** `stage0 install vm1
--image rocky-10.2-vm` installs Rocky's aarch64 cloud image in a QEMU VM on
the private test network and sees it up with SSH, hostname `vm1` and the
site's keys from NoCloud, in 96 s ([`docs/testbed.md`](docs/testbed.md)).

## Open decisions
(none)

## Next session
1. IPv6 netboot on the Pi 5 works up to the agent
   ([`docs/netboot.md`](docs/netboot.md#ipv6-netboot-on-a-pi-5)): serve
   must run from `/usr/local/bin/stage0` (`tools/install-capped.sh` after a
   rebuild) for router advertisements and neighbour entries. k52 has
   `USE_IPV6=1` in its EEPROM.
2. The agent over IPv6 (kernel SLAAC on link up, then HTTP to
   `[fd00:10:3::1]`), so an install needs no IPv4; then `install` choosing
   IPv6 URLs.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
