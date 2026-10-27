# QEMU test bed

How a QEMU VM is installed by stage0 on the host's private test network
(pilab: aarch64, KVM). One command does it:

```
$ ./stage0 install vm1 --image rocky-10.2-vm     # 128 s on pilab
```

`site.yaml` names the VM (`vm: {mac: …}`, optional `memory`, `cpus`,
`diskGB`, `firmware`) and its network (`vnet`); the image config
(`images/rocky-10.2-vm.yaml`) gives the cloud image (qcow2), the agent's
`kernel` and `initrd`, and `nocloud: true`. `install` builds the agent's
UKI, runs the switch with the tap as a port, serves, starts QEMU to
netboot it with UEFI (console on a Unix socket QEMU waits for), follows
its console, stops it after `STAGE0-INSTALLED`, starts it again from the
disk with the NoCloud pointer, and waits for the login prompt, the lease and
ping and SSH. The VM's files (disk, UEFI variables, console socket) are in
`~/.cache/stage0/vm/`. The VM is stopped when `install` exits, since its
network lives in the `install` process; with `--keep`, `install` keeps the
switch, serve and the VM running until interrupted (or `--timeout`), so
stage 1 can be pointed at it.

`tools/test-vm.sh [node] [image]` is the end-to-end check for CI: it
builds stage0 and runs that install, printing its JSON result and exiting
non-zero if it fails (135 s on pilab, building included).

The sections below are the pieces, and how they were first run by hand.

> Last verified: 2026-10-27

## The network

- **`stage0tap0`**: a tap device owned by mtim, from
  `/etc/systemd/network/stage0tap0.netdev` and `.network`; the host's side
  is `10.0.9.1/24` and `fd00:10:9::1/64`, connected to nothing real.
  `site.yaml` calls it `vnet` (`prefix6: fd00:10:9::/64`).
- **`stage0 vswitch --socket PATH`**: a learning switch that VMs join with
  `-netdev stream,id=n0,server=off,addr.type=unix,addr.path=PATH`; the tap
  is one more port, so `stage0 serve vnet` answers the VMs.

## Booting the agent: UEFI netboot of a UKI

The VM netboots like a real UEFI machine (2026-10-27; until then QEMU
loaded the agent itself with `-kernel`/`-initrd`):

- **Firmware:** Ubuntu's `AAVMF_CODE.no-secboot.fd` and `AAVMF_VARS.fd`
  (package `qemu-efi-aarch64`, Debian's can't netboot:
  [`netboot.md`](netboot.md#qemu-aarch64)), which stage0 fetches by pinned
  URL and SHA-256 into `~/.cache/stage0/firmware/` unless the node names
  its own `firmware`. QEMU boots the NIC first (`bootindex`) for the agent,
  the disk first afterwards.
- **HTTP boot:** for the agent QEMU turns the firmware's PXE off
  (`-fw_cfg name=opt/org.tianocore/IPv4PXESupport,string=n`, and
  `IPv6PXESupport`), so it goes straight to UEFI HTTP boot (class
  `HTTPClient:Arch:00019`, option 93 = 19); an unanswered PXE try first
  costs over a minute. stage0's DHCP answers with class `HTTPClient` and
  the boot file `http://10.0.9.1/uki.efi`; serve's HTTP sent the 265 MB in
  9.9 s (27 MB/s).
- **PXE (TFTP)** still works for firmware that only does PXE: an aarch64
  UEFI client (class `PXEClient:Arch:00011`, option 93 = 11) gets a plain
  offer naming `uki.efi`; with the Pi's `PXEClient` class but no option
  43, edk2 waits for a boot server (BINL) and reports "No valid offer
  received". It first asks the file's size and aborts ("User aborted the
  transfer"), then fetches it: 265 MB in 16 s (16 MB/s) over stage0's
  TFTP.
- **The UKI** (`internal/uki`), built by stage0: systemd's EFI stub
  (Debian's `systemd-boot-efi`, fetched by pinned URL and SHA-256; Debian's
  pool keeps only the current version, so a point release means a new pin)
  with sections `.osrel`, `.cmdline` (the agent's plan), `.initrd` and
  `.linux`. The kernel is Rocky 10's stock arm64 pxeboot `vmlinuz`
  (`https://dl.rockylinux.org/pub/rocky/10/BaseOS/aarch64/os/images/pxeboot/`),
  an EFI zboot wrapper ("zimg", gzip) that stage0 unwraps to the plain
  `Image`. The initrd is that directory's `initrd.img`, padded to 4 bytes,
  with stage0's initramfs appended; the kernel unpacks both, and stage0's
  `/init` replaces dracut's.
- EL builds `virtio_net` and `virtio_blk` as modules (they are `.ko.xz`
  files in that initrd), so the agent loads drivers itself: every device's
  modalias under `/sys`, handed to the initrd's `modprobe`, and asks for
  an address itself (DHCP with vendor class `stage0-agent`), set over
  netlink.
- **Image:** EL publishes aarch64 cloud images only as qcow2. The agent
  takes a qcow2 payload as it takes xz: fetched into RAM, checked against
  `stage0.sha256=`, then written with only its data clusters.

Result (Rocky 10.2 GenericCloud aarch64, 447 MB qcow2, 10 GiB disk): the
image fetched in 15 s and written in 20 s; up and answering SSH 128 s
after QEMU started.

## Over IPv6

`stage0 install vm1 --image rocky-10.2-vm --ipv6` (2026-10-27): stage0's
DHCPv4 leaves the VM's boot requests unanswered, so the firmware's HTTP
boot over IPv4 times out (about 140 s: the install took 270 s instead of
128 s) and it tries IPv6. Its DHCPv6 (DUID-LLT, architecture 19) gets an
address and the boot file `http://[fd00:10:9::1]/uki.efi` with vendor
class `HTTPClient` (enterprise 343); the UKI came in 10.9 s. The agent's
plan names `[fd00:10:9::1]`, so it takes a SLAAC address and fetches the
image over IPv6. The installed system then gets its IPv4 lease.

## Booting the installed disk

The same VM, the disk first in the boot order, boots to `localhost login:`
(`vm1 login:` with NoCloud), takes its address from stage0 (the same
lease) and its SSH server greets (`SSH-2.0-OpenSSH_9.9`).

## Hostname and keys: cloud-init NoCloud from `serve`

The cloud image's cloud-init takes NoCloud data over HTTP when QEMU's
SMBIOS serial points at it, and `serve` serves plain files, so no code is
involved:

- `root/nocloud/vm1/meta-data`: `instance-id: vm1-1`, `local-hostname: vm1`
- `root/nocloud/vm1/user-data`: `#cloud-config`, `hostname: vm1`,
  `disable_root: false`, and root's `ssh_authorized_keys` (the site's)
- QEMU: `-smbios "type=1,serial=ds=nocloud;s=http://10.0.9.1/nocloud/vm1/"`

On a later boot of the already-installed disk, cloud-init fetched both
files (and asked for the optional `network-config` and `vendor-data`), the
console showed `vm1 login:`, and root logged in over SSH with the site's
keys. The VM needs its address from stage0 first: `serve` answers an
installed node's plain DHCP only if an install record names its MAC
(`~/.local/state/stage0/nodes/`; written by hand for this run, as
`install` doesn't drive VMs yet).
