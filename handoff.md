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
- **WW1** How stage0 gets `CAP_NET_RAW` for router advertisements: a
  setcap'd copy installed once, a small helper, or skip RAs. Your `sudo`
  "ask" rule prompts for every setcap after a rebuild.
- **WW2** What VLAN `install` should move a node's port to afterwards
  (stage 0.5); the head also needs to stay reachable from stage0.

- **XX1** How a QEMU VM boots stage0's agent, since Debian's UEFI firmware
  can't netboot ([`docs/netboot.md`](docs/netboot.md#qemu-aarch64)):
  (a) QEMU loads the agent's kernel and initramfs directly (`-kernel`,
  `-initrd`), skipping firmware netboot in VMs (TFTP stays tested on the
  Pis); (b) fetch a UEFI build with networking (e.g. upstream EDK2 or
  Fedora's `edk2-aarch64`); (c) iPXE for arm64. Recommendation: (a) now,
  (b) later. Either way the agent needs a generic arm64 kernel (the Pi
  kernels don't run on QEMU's `virt`), e.g. from Rocky's or Alma's
  aarch64 cloud image.

## Next session
1. Milestone 2, the QEMU test bed: the virtual switch is built
   (`internal/vswitch`); next is a command that runs it with stage0 as a
   port (tap, then gVisor; JJ1), and booting a VM's agent (XX1).
2. [`ROADMAP.md`](ROADMAP.md#next): Ubuntu for Pi, Alma and Raspberry Pi
   OS on the other board.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
