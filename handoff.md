# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**Three images install and come up with SSH,** on the Pi 5 (k52) and the
Pi 400 (k400): AlmaLinux 10.1 and Raspberry Pi OS Lite trixie on both,
Rocky 10.2 on the Pi 5 only (it doesn't boot a Pi 4; see
[`docs/images.md`](docs/images.md)). Each ends at its stage0 lease with the
site's keys and its hostname, with or without a serial console. k52 runs
Alma (`10.0.3.100`), k400 Raspberry Pi OS (`10.0.3.101`); both on.

## Open decisions
(none)

## Next session
1. Milestone 2, booting a VM's agent (XX1, decided): `stage0 vswitch` and
   the `vnet` network (tap `stage0tap0`, 10.0.9.1/24) are built. Next:
   - a qcow2 reader in Go: EL publishes aarch64 cloud images only as
     qcow2 (Rocky, Alma); convert to raw in the cache;
   - the agent on Rocky's pxeboot `vmlinuz` + `initrd.img`
     (`/BaseOS/aarch64/os/images/pxeboot/`): stage0's cpio appended after
     the initrd, and the agent loads the modules the hardware asks for with
     the initrd's `modprobe` (EL builds virtio as modules);
   - QEMU boots it with `-kernel`/`-initrd`, NIC on the vswitch; installs a
     cloud image to a virtual disk, which then boots with UEFI (AAVMF; no
     netboot needed for a disk).
2. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
