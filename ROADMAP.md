# Stage0 Roadmap

Planned, not built. What is built is described in [`docs/`](docs/index.md)
(the install as it works today: [`docs/install.md`](docs/install.md)); as
each part below is built, its design moves there and leaves this file.

## Goal

Stage0 installs **one head node** from nothing but this repo and a connection
to the provisioning network: a base OS (EL 9+, Debian 12+, Ubuntu 24.04+;
systemd assumed) on disk with its own bootloader, reachable over SSH, close to
stock. "Stage 1" (the provisioner under test) takes over from there. In a real
site the same job is a person with a USB stick and a crash cart; stage0 exists
so different stage-1 provisioners can be CI-tested on real and virtual hardware.

## Stages

| Step | Does | State (Pi 5) |
| --- | --- | --- |
| 0.0 | Load site config; fetch pinned artifacts; power off the target, move its port to the provisioning VLAN; start services; log the console | Built (`stage0 install`), except moving the port: it checks the port is on the VLAN |
| 0.1 | Target netboots (TFTP or HTTP) | Built (IPv4 TFTP) |
| 0.2 | The agent identifies the node and gets its plan | Built: the plan is the node's `cmdline.txt`, written by `install` |
| 0.3 | Write the image to disk, then the first-boot files | Built |
| 0.4 | First boot from disk; hostname, network, SSH key set; node reports "up" (console prompt, or lease plus ping and SSH) | Hostname and root's SSH key set (kernel command line); IPv4 lease from stage0 and IPv6 link-local, both printed; root filesystem not grown; "up" is the console's login prompt |
| 0.5 | Mark the node installed (stop answering its PXE), stop | Built; the port stays on the provisioning VLAN (the head provisions stage 1 on another VLAN; decided WW2) |

Boot order stays **network first**: no answer means the node falls through to
disk, which is safe for Pis and in general. Stage0 must therefore stop
answering a node once it is installed, or it would reinstall it on every
reboot.

## Next

1. **First-boot settings (stage 0.4), the rest:** hostname and root's SSH
   key are built (systemd settings on the kernel command line; see
   [`docs/install.md`](docs/install.md#first-boot-settings)). Installed
   nodes keep their IPv4 lease from stage0 (OO1, built) and get an IPv6
   address from the network's prefix (router advertisements, built). Deferred:
   - **Growing the root filesystem (OO2):** left to stage 1 for now ("close
     to stock"); Rocky ships `rootfs-expand` and `growpart`.
   - **The seed's `root/` directory (GG2, decided):** for what the command
     line can't carry, e.g. a static address; built when first needed.
   - cloud-init NoCloud served by `serve`'s HTTP
     (`ds=nocloud;s=http://…/`), for images that have cloud-init (Rocky Pi
     does not).
2. **More images:** Ubuntu for Pi. Built: Rocky 10.2 (Pi 5; doesn't boot
   a Pi 4), AlmaLinux 10.1 and Raspberry Pi OS Lite trixie (both on Pi 5
   and Pi 400).
## Install routes

The agent writes a stock disk image (xz, checked by SHA-256) to the disk and
then lays first-boot files on top (decided: HH1). Two ways to get the image
to the agent:

- **HTTP** after the kernel brings up the network (built; the default).
- **Inside the initramfs**, so Linux never touches the network (built; kept
  for nodes where that matters). Limited by the Pi bootloader's 60 s per
  TFTP file, about 180 MB here. To try: raise `TFTP_FILE_TIMEOUT` in the
  EEPROM, or split the image over several initramfs files if `config.txt`
  allows a list (both unverified).

Later, for UEFI and QEMU machines: a stock installer ISO with an answer file
(kickstart, preseed, autoinstall). Distro installers don't netboot a Pi 5.
Later, if custom partitioning or non-raw images are needed: the agent
builds a root filesystem in RAM (from a container built here, or the
image's partitions mounted) and runs the target's own tools from a chroot
to partition, format, copy and install the bootloader (LL1).

**Container route (A, decided BA1–BA3): built** for Pis, over IPv4 or
IPv6 ([containers](docs/containers.md)). A container comes from a
registry, podman's local store (`podman:<image>`) or a saved layout; its
layout from the node, the image config or the labels (BB1), as a
partition list or an Ignition config that the installer's Ignition
applies; private registries with logins in `.secrets`. Later, bootc (E) for UEFI machines,
testable in QEMU with Ubuntu's firmware
([netboot](docs/netboot.md#qemu-aarch64)).

## Components (one static Go binary)

Go because every needed library is maintained in Go (pin/tftp,
insomniacslk/dhcp, go-containerregistry, go-diskfs, go-ipmi with SOL,
go-serial) and `../lab`'s `chunk` already used that stack. Python lost its TFTP
base (fbtftp, archived 2022-11); Rust has no DHCPv6 server.

Built: site config, switch control, serial consoles, `serve` (DHCPv4, TFTP,
HTTP), the agent, initramfs and seed builders (see
[`docs/development.md`](docs/development.md)). Planned:

- **Site config:** credentials live in `.secrets` (git-ignored) and the
  config refers to them by name. Still to come: defaults for everything but
  the target, per-image config files.
- **Network modes:** *own* (built for IPv4; DHCPv6 and RA to come), *proxy*
  (DHCPv4 alongside an existing server), *boot-only* (DHCPv6 boot URL
  alongside an existing SLAAC router), *none* (firmware or EEPROM configured
  with a static IP or SLAAC and a fixed boot server).
- **Boot services:** UEFI HTTP boot, a node API, a read-only OCI registry
  cache pinned by digest (images from a registry).
- **Control:** power (QEMU QMP, IPMI), console (QEMU serial, IPMI SOL;
  always logged), interactive console input for people.
- **Firmware:** a netbooted EEPROM updater (the bootloader asks for
  `pieeprom.sig` over TFTP while netbooting), so a node needn't run
  Raspberry Pi OS to be updated; it needs the node's EEPROM settings from
  somewhere (site.yaml, or read by the agent) to carry them over. The free
  nodes were updated with Raspberry Pi's tools ([`docs/netboot.md`](docs/netboot.md#bootloader-eeprom-updates)).
- **Test bed (GG3, decided):** QEMU VMs (x86_64 q35 + OVMF, aarch64 virt +
  AAVMF) on a virtual Ethernet switch built into stage0, which QEMU joins
  with `-netdev stream` over a Unix socket. stage0 is a port on that switch
  through a user-owned tap device (default: one-time root setup, `serve`
  runs unchanged) or gVisor's netstack (no root); both are built (JJ1).
  Then: the switch attaches a real interface (e.g. eth0.3) as a port with a
  packet socket (`CAP_NET_RAW`), so VMs and Pis share a network without a
  host bridge. Frames it sends out don't reach the host's own stack, so in
  that mode stage0 serves through its netstack port with its own address,
  and refuses to run beside a `serve` on the same interface. The host itself
  can't hold a conversation with VMs in that mode (replies go to the wire);
  add a tap port if that's wanted. A virtual BMC in front of QEMU tests the IPMI client and SOL
  without hardware.

## Milestones (order tentative)

1. **Pi 5 head node over IPv4:** `stage0 install k52` installs Rocky 10.2
   and leaves it reachable over SSH with its hostname and key. Done.
2. QEMU test bed on the virtual switch; the same install for an aarch64 VM.
   Done (tap port; `install` drives VMs, `--keep` keeps one up;
   `tools/test-vm.sh` runs it for CI). Deferred (YY1): the gVisor port,
   until a test bed machine without root needs it; then current gVisor,
   which needs Go 1.27.3 or later (stage0 is on 1.26). The raw-socket
   port waits for it, as stage0 must serve through a netstack port there.
3. Pi 5 over IPv6. Done (`install --ipv6`, k52).
4. **Pis' virtual BMC:** done (`labctl bmc`: IPMI 2.0 with power by PoE and
   SOL by the serial console, [bmc](docs/bmc.md); `stage0 get nodes -o
   json` for stage 1).
5. Ignition for container partitioning, and first-boot configuration by
   stage0, Ignition or cloud-init (BI1): done
   ([containers](docs/containers.md#partitioning-by-ignition),
   [first boot](docs/firstboot.md)).
6. UEFI and x86_64: aarch64 VMs netboot the agent as a UKI by UEFI HTTP
   boot, or PXE (done, [test bed](docs/testbed.md)); next x86_64, the
   installer ISO route, an IPMI client against real BMCs (in another lab).
7. Network modes beyond *own* (BH1): done — proxy (IPv4 and IPv6, VM and
   Pis) and none (k400) ([network modes](docs/network-modes.md)).

Dropped (BE1): install-once state and workflow plans (nothing needs
them); state export is `stage0 get nodes -o json`, above. Dropped (BK2,
BM2): a resident serve that installs hand work to; `stage0 serve` stays
optional, for mode own, to keep installed nodes' addresses.

## Out of scope

- **Booting through BMC virtual media.** The rig (the tool agents and CI use)
  already attaches and boots an ISO over NFS. A future configuration will use
  stage0 to install the system that runs the BMC and NFS side.
