# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**Pis:** three images install and come up with SSH on the Pi 5 (k52) and
the Pi 400 (k400) (Rocky 10.2: Pi 5 only); k52 runs Alma (`10.0.3.100`),
k400 Raspberry Pi OS (`10.0.3.101`), both on. **VMs:** run by hand, stage0
installed Rocky's aarch64 cloud image in a QEMU VM on the private test
network, and the disk boots with UEFI to a login prompt with SSH
([`docs/testbed.md`](docs/testbed.md)).

## Open decisions
(none)

## Next session
1. `stage0 install` for VM nodes: site config for a VM (QEMU: memory,
   disk file, MAC, the vswitch socket), power = starting and stopping the
   QEMU process, the agent kernel and initrd from an image config (the
   pxeboot files and their checksums from `.treeinfo`), the console from
   QEMU's serial.
2. cloud-init NoCloud served by stage0 for cloud images (hostname, SSH
   keys): QEMU `-smbios type=1,serial=ds=nocloud;s=http://10.0.9.1/…/`.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
