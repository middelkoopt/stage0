# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-27)
**Pis:** four disk images install and come up with SSH (Rocky 10.2: Pi 5
only; Ubuntu 26.04: Pi 5 tried). **Containers:** the Rocky container
installs on the Pi 5 and Pi 400 from podman's local store, a registry or
a saved layout (`container:`). **VMs:** `stage0 install vm1 --image rocky-10.2-vm`
netboots the agent as a UKI by UEFI HTTP boot (128 s;
[`docs/testbed.md`](docs/testbed.md)). **IPv6:** `install --ipv6` runs
over IPv6 alone for disk images, containers and VMs. k52 runs the Rocky
container (installed over IPv6), k400 Rocky from the container.
**BMC:** `stage0 bmc` answers IPMI for the Pis: power by PoE, SOL by the
serial console.

## Open decisions
(none)

## Next session
1. Network modes: proxy works (VM, k400); next boot-only (IPv6) and none
   ([`docs/network-modes.md`](docs/network-modes.md)).
2. First-boot configuration is built ([`docs/firstboot.md`](docs/firstboot.md)):
   stage0, Ignition or cloud-init per node or image. VMs with cloud-init
   untested.
3. Nodes: k52 runs Ubuntu (cloud-init first boot test); k400 the Rocky
   container, installed in proxy mode, EEPROM now `USE_IPV6=0`. The Rocky container
   now has SELinux (enforcing), cloud-init and Ignition. k53 is powered
   off; it may be powered and used as root, stopping (not disabling) its
   services. Its `sudo` needs a password stage0 doesn't have.
