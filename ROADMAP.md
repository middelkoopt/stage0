# Stage0 Roadmap

Planned, not built. Nothing here exists yet. As each part is built, its design
moves to `docs/` and leaves this file. Platform facts behind these choices are
in [`docs/netboot.md`](docs/netboot.md).

## Goal

Stage0 installs **one head node** from nothing but this repo and a connection
to the provisioning network: a base OS (EL 9+, Debian 12+, Ubuntu 24.04+;
systemd assumed) on disk with its own bootloader, reachable over SSH, close to
stock. "Stage 1" (the provisioner under test) takes over from there. In a real
site the same job is a person with a USB stick and a crash cart; stage0 exists
so different stage-1 provisioners can be CI-tested on real and virtual hardware.

## Stages

| Step | Does |
| --- | --- |
| 0.0 | Load site config; pull or build pinned artifacts; power off the target, move its switch port to the provisioning VLAN; start services; log the console |
| 0.1 | Target netboots (TFTP or HTTP) |
| 0.2 | Installer environment identifies the node and fetches its plan |
| 0.3 | Install using the target OS's own tools (partition, format, bootloader) |
| 0.4 | First boot from disk; cloud-init (or equivalent) sets identity, network, SSH keys; node reports "up" |
| 0.5 | Mark the node installed (stop answering it), move its port to the cluster VLAN, stop |

Boot order stays **network first**: no answer means the node falls through to
disk, which is safe for Pis and in general. Stage0 must therefore stop
answering a node once it is installed, or it would reinstall it on every
reboot.

## Components (one static Go binary)

Go because every needed library is maintained in Go (pin/tftp,
insomniacslk/dhcp, go-containerregistry, go-diskfs, go-ipmi with SOL,
go-serial) and `../lab`'s `chunk` already used that stack. Python lost its TFTP
base (fbtftp, archived 2022-11); Rust has no DHCPv6 server.

The boot artifact is one kernel + initramfs per platform: a UKI on UEFI, a Pi
file set on Pi (a Pi cannot boot a UKI).

- **Site config** (`site.yaml`): every field defaulted except the target;
  inventory nodes get extra per-node settings. Credentials live in `.secrets`
  (git-ignored) and the config refers to them by name.
- **Network modes:** *own* (DHCPv4, DHCPv6, RA), *proxy* (DHCPv4 alongside an
  existing server), *boot-only* (DHCPv6 boot URL alongside an existing SLAAC
  router), *none* (firmware or EEPROM configured with a static IP or SLAAC and
  a fixed boot server).
- **Boot services:** dynamic TFTP (Pi first), HTTP (UEFI HTTP boot, ISO and
  image downloads, node API), read-only OCI registry cache pinned by digest.
- **Control:** power (QEMU QMP, IPMI), console (QEMU serial, IPMI SOL;
  always logged), and console input for people. PoE power, switch VLANs and
  local serial capture are built (see
  [`docs/development.md`](docs/development.md)); new control follows the same
  CLI pattern.
- **Test bed:** QEMU (x86_64 q35 + OVMF, aarch64 virt + AAVMF) on a socket
  network, with a virtual BMC in front of QEMU so the IPMI client and SOL are
  tested without hardware.

## Install routes

Image handling (decompress, partition tables, filesystems, cpio) is done in
Go, not with host tools.

Both routes give the installer a root filesystem to chroot into, so the disk
work is done by the target OS's own tools either way. Built together.

1. **Container built here:** layers unpacked into RAM.
2. **Stock image:** the distro's own partition images (e.g. a Pi image's root
   partition) shipped as raw images inside an OCI artifact and mounted, not
   unpacked into files.

Decided for the Pi 5:
- The installer runs the **target image's own kernel and initramfs**, with the
  stage0 agent added (modules match; close to stock).
- **Target disk:** NVMe if present, otherwise named in the site config.
- **First-boot settings:** cloud-init seed where the image has cloud-init,
  files written from the chroot otherwise.
- **First image:** Rocky 10 SBC RaspberryPi (no UEFI shim), used as shipped;
  one config file per distro image.
- **Unknown machines** install only when stage0 runs with `--install-any`, and
  then exactly one.

A third route, stock installer ISO + answer file, is possible later for UEFI
machines; distro installers do not netboot a Pi 5.

## Milestones (order tentative)

1. **Pi 5 over IPv4 TFTP:** dynamic TFTP, both install routes; a Pi 5 head
   node up with SSH.
2. Pi 5 over IPv6.
3. Network modes beyond *own*: none, boot-only, proxy.
4. UEFI and QEMU (x86_64, aarch64): HTTP boot, QEMU power and console, IPMI
   client with SOL against a virtual BMC; CI test bed.
5. Install-once state, workflow plans, optional state export for stage 1.

## Later goals

- The switch as a virtual BMC: IPMI (power, SOL) backed by PoE and the
  serial console, so stage-1 tools can use one interface everywhere.

## Out of scope

- **Booting through BMC virtual media.** The rig (the tool agents and CI use)
  already attaches and boots an ISO over NFS. A future configuration will use
  stage0 to install the system that runs the BMC and NFS side.
