# QEMU test bed

How a QEMU VM is installed by stage0 on the host's private test network
(pilab: aarch64, KVM). One command does it:

```
$ ./stage0 install vm1 --image rocky-10.2-vm     # 96 s on pilab
```

`site.yaml` names the VM (`vm: {mac: …}`, optional `memory`, `cpus`,
`diskGB`, `firmware`) and its network (`vnet`); the image config
(`images/rocky-10.2-vm.yaml`) gives the cloud image (qcow2), the agent's
`kernel` and `initrd`, and `nocloud: true`. `install` runs the switch with
the tap as a port, serves, starts QEMU to boot the agent (`-kernel`,
`-initrd`, console on a Unix socket QEMU waits for), follows its console,
stops it after `STAGE0-INSTALLED`, starts it again from the disk with UEFI
and the NoCloud pointer, and waits for the login prompt, the lease and
ping and SSH. The VM's files (disk, UEFI variables, console socket) are in
`~/.cache/stage0/vm/`. The VM is stopped when `install` exits, since its
network lives in the `install` process; with `--keep`, `install` keeps the
switch, serve and the VM running until interrupted (or `--timeout`), so
stage 1 can be pointed at it.

The sections below are the pieces, and how they were first run by hand.

> Last verified: 2026-10-26

## The network

- **`stage0tap0`**: a tap device owned by mtim, from
  `/etc/systemd/network/stage0tap0.netdev` and `.network`; the host's side
  is `10.0.9.1/24`, connected to nothing real. `site.yaml` calls it `vnet`.
- **`stage0 vswitch --socket PATH`**: a learning switch that VMs join with
  `-netdev stream,id=n0,server=off,addr.type=unix,addr.path=PATH`; the tap
  is one more port, so `stage0 serve vnet` answers the VMs.

## Booting the agent

Debian's UEFI firmware for QEMU can't netboot
([`netboot.md`](netboot.md#qemu-aarch64)), so QEMU loads the agent
directly (`-kernel`, `-initrd`):

- **Kernel:** Rocky 10's stock arm64 pxeboot `vmlinuz`
  (`https://dl.rockylinux.org/pub/rocky/10/BaseOS/aarch64/os/images/pxeboot/`).
- **Initrd:** that directory's `initrd.img` with stage0's initramfs
  (`stage0 build initrd`) appended (`cat initrd.img agent.cpio.gz`); the
  kernel unpacks both, and stage0's `/init` replaces dracut's.
- EL builds `virtio_net` and `virtio_blk` as modules (they are `.ko.xz`
  files in that initrd), so the agent loads drivers itself: every device's
  `modalias` under `/sys`, handed to the initrd's `modprobe`. The kernel's
  `ip=dhcp` runs before that, so the agent asks for an address itself
  (DHCP with vendor class `stage0-agent`, which stage0 answers like a PXE
  client, without boot options) and sets it over netlink.
- **Image:** EL publishes aarch64 cloud images only as qcow2. The agent
  takes a qcow2 payload as it takes xz: fetched into RAM, checked against
  `stage0.sha256=`, then written with only its data clusters.

The run (Rocky 10.2 GenericCloud aarch64, 447 MB qcow2, 10 GiB disk):

```bash
./stage0 build initrd --out agent.cpio.gz
cat initrd.img agent.cpio.gz > initrd
truncate -s 11G disk.raw
./stage0 vswitch --socket vnet.sock &
./stage0 serve vnet --root root &          # root/images/<the .qcow2>
qemu-system-aarch64 -M virt -accel kvm -cpu host -m 2048 -smp 2 \
  -display none -serial file:agent.log \
  -kernel vmlinuz -initrd initrd \
  -append "console=ttyAMA0 stage0.disk=vda stage0.sha256=<sum> stage0.url=http://10.0.9.1/images/<file> stage0.report=http://10.0.9.1/stage0/report" \
  -drive file=disk.raw,if=virtio,format=raw \
  -netdev stream,id=n0,server=off,addr.type=unix,addr.path=vnet.sock \
  -device virtio-net-pci,netdev=n0,mac=52:54:00:12:34:56
```

Result: drivers for 51 devices, `10.0.9.100` by DHCP, the image fetched in
13 s and written in 24 s (`STAGE0-INSTALLED`).

## Booting the installed disk

The same VM with UEFI firmware instead of `-kernel` (`-drive
if=pflash,…,file=/usr/share/AAVMF/AAVMF_CODE.fd` and a writable copy of
`AAVMF_VARS.fd`) boots the disk to `localhost login:`, takes its address
from stage0 (the same lease) and its SSH server greets
(`SSH-2.0-OpenSSH_9.9`).

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
