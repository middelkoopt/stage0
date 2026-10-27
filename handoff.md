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
- **AC1** IPv6 netboot's next step needs `CAP_NET_ADMIN` as well as
  `CAP_NET_RAW` on `/usr/local/bin/stage0` (for a static neighbour entry):
  OK to add it to `tools/install-capped.sh`'s `setcap`? That script runs
  without a prompt, so this widens what runs unprompted.

## Next session
1. IPv6 netboot on the Pi 5 (AB2: the human said "see what you can do"):
   [`docs/netboot.md`](docs/netboot.md#ipv6-netboot-on-a-pi-5-experimental-not-working-yet).
   DHCPv6 works; the bootloader never answers neighbour solicitations for
   its DHCPv6 address, so the host can't reply over TFTP. Plan: when
   dhcp6d leases an address, stage0 adds a static neighbour entry for it
   (netlink RTM_NEWNEIGH, MAC from the client's DUID-LL; needs
   CAP_NET_ADMIN, AC1), then retry. Live test: `tools/install-capped.sh`,
   then `/usr/local/bin/stage0 serve pinet --root ~/.cache/stage0/run/k52`,
   `./stage0 power cycle k52`, follow `./stage0 console k52`. k52 has
   `USE_IPV6=1` in its EEPROM (falls back to IPv4/SD fine).
2. Then the agent over IPv6 (kernel SLAAC, HTTP to `[fd00:10:3::1]`).
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
