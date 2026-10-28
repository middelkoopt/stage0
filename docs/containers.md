# Installing from a container

How `stage0 install` installs a container image (route A), and the facts
it rests on, as seen on k52. The container is
[`containers/rocky10-rpi`](../containers/rocky10-rpi/Containerfile).

> Last verified: 2026-10-27

## How it works

**Works on a Pi 5 and a Pi 400** (k52, k400, 2026-10-27): `stage0 install
k52 --image rocky10-rpi-container` installed Rocky 10.2 in 45 s; it was up
197 s after power-on, root logging in with the site's keys. On k400 the
install took 93 s, and Rocky booted from SD, which Rocky's own image
doesn't on a Pi 400 ([images](images.md)): the bootloader matched the
boot partition's basic-data GPT type. Its GPIO console stayed silent
without `enable_uart=1`, now in the image config.

1. **The container** comes from a registry or a saved layout. An image
   config names it with `container:` and gives the installed system's
   `cmdline`
   ([`images/rocky10-rpi-container.yaml`](../images/rocky10-rpi-container.yaml)):
   - **A registry reference** (it has a `/`), e.g.
     `quay.io/org/rocky10-rpi:10`: stage0 (`internal/registry`) pulls it
     into `~/.cache/stage0/oci/registry/<manifest digest>/` on each
     install (the tag resolved again; blobs already there kept), choosing
     linux/arm64 from a multi-arch index. Anonymous, with the bearer token
     public registries ask for; `localhost` registries are plain HTTP, as
     with podman. Verified 2026-10-27: Alpine from Docker Hub, Rocky from
     quay.io, and k52 installed from a local `registry:2` the Rocky Pi
     container was pushed to (`podman push --tls-verify=false … 
     localhost:5000/stage0/rocky10-rpi:latest`). Nodes never reach the
     registry: stage0 serves what it pulled.
   - **An image in podman's local store**, `podman:<image>`, e.g.
     `podman:localhost/stage0/rocky10-rpi`: stage0 runs `podman image save
     --format oci-dir` into `~/.cache/stage0/oci/podman/<image ID>/`, so
     each rebuild is picked up and the same image saved once (k52,
     2026-10-27). For images built here; a registry suits images built
     elsewhere (CI).
   - **A layout name**, e.g. `rocky10-rpi`: `podman build -t
     stage0/rocky10-rpi containers/rocky10-rpi`, then `podman save
     --format oci-dir -o ~/.cache/stage0/oci/rocky10-rpi
     localhost/stage0/rocky10-rpi`.
2. **The layout** — partitions (`name`, `size`, `type` vfat or ext4,
   `path`; the first is the Pi's boot partition, the last the root,
   filling the disk) and the installer (`kernel`, `initramfs`: paths in
   the image) — comes from, most specific first: the node's `partitions:`
   in `site.yaml`; the image config's `partitions:` and `installer:`; the
   container's labels `pi.provision.partition` and
   `pi.provision.installer` (JSON). Optional labels: `pi.provision.hosts`
   (lines of `/etc/hosts`) and `pi.provision.resolv` (where
   `/etc/resolv.conf` points). Seen on k52 (2026-10-27): a node layout
   adding an 8 GiB ext4 `/var` gave three partitions and the system ran.
3. **stage0** (`internal/oci`, `internal/container`) reads the layout in Go,
   no container runtime: it takes the boot partition's files and the
   installer initramfs from the layers, appends its agent and a dracut
   hook to the initramfs, and serves the OCI layout under `oci/` and the
   resolved partition layout as `layout.json`. The kernel command line
   carries the manifest's digest; the agent checks every blob against its
   digest from there down.
4. **The agent**, run by the hook once dracut's network is up, partitions
   with the installer's `sgdisk` (GPT, partitions named as labelled),
   formats with `mkfs.vfat`/`mkfs.ext4`, mounts the ext4 ones in the
   root's tree, unpacks the layers onto it (owners, modes, links, xattrs;
   whiteouts applied), copies the FAT partitions' files onto them (FAT
   keeps no owners or links), writes `/etc/fstab` (by `PARTLABEL`), hosts
   and resolv.conf, applies the first-boot configuration and labels the
   files for SELinux when the image has a policy
   ([first boot](firstboot.md)), then seeds the boot partition as for disk
   images.

Not yet: VMs, private registries (credentials).

## Partitioning by Ignition

**Works on a Pi 5** (k52, 2026-10-27, `images/rocky10-rpi-ignition.yaml`
with `images/rocky10-rpi-var.ign`: boot, a 4 GiB `/var`, root): the
installer's own Ignition makes the partitions and filesystems, and the
agent only mounts them and unpacks; up in 208 s.

- **Giving it:** `ignition: <file.ign>` (Ignition spec 3 JSON; write
  Butane and convert with `butane`) on a node in `site.yaml` or in an image
  config, or the label `pi.provision.ignition`, in place of `partitions:`
  / `pi.provision.partition`, with the same precedence. stage0 reads the
  layout from its `storage` (the one disk's partitions, each with a vfat or
  ext4 filesystem on `/dev/disk/by-partlabel/<label>` and a mount path) to
  know where to mount; raid, luks, several disks and other formats are
  refused.
- **Serving it:** stage0 serves the config as `ignition.ign` with the
  disk's `device` set to the node's disk, `wipeTable` and every
  `wipeFilesystem` on (a fresh install: new partitions at old offsets would
  otherwise keep old filesystems), and FAT partitions without a `typeGuid`
  typed Microsoft basic data: Ignition's default type, Linux filesystem,
  left k52 with "GPT: No bootable partitions". The installer's command line
  gets `ignition.firstboot ignition.platform.id=metal
  ignition.config.url=<base>/ignition.ign`; `layout.json` says
  `"ignition": true`, so the agent skips sgdisk and mkfs and waits for the
  partitions by label.
- **The container** has Ignition (2.26, Rocky's) in its installer
  initramfs (`dracut --add "network-manager ignition"`); its dracut module
  wants `mlxbf_bootctl` on aarch64 (NVIDIA BlueField), which a Pi kernel
  lacks, so the Containerfile makes it optional. Without
  `ignition.firstboot` Ignition does nothing.
- **Ordering:** Ignition's fetch, kargs and disks stages run (disks took
  about 22 s on k52), but dracut's pre-mount hooks didn't wait for them:
  the agent tried to mount the root while Ignition formatted it ("device or
  resource busy"). The appended archive carries a systemd drop-in,
  `dracut-pre-mount.service.d/90-stage0.conf` with
  `After=ignition-disks.service`. Ignition's mount and files stages wait
  for a `/sysroot` the installer never mounts, so files, users and units
  in the config aren't applied.
- **The console garbles the agent's lines** while systemd prints its
  status: the console copy of the report replaced the good HTTP copy, so
  stage0 found no MAC, didn't mark the node installed, and the reboot
  netbooted the installer again. `install` now takes only reports and
  results that are valid JSON.

## The installer initramfs

- **Built in the container** with `dracut --no-hostonly --add
  network-manager --install "sgdisk mkfs.ext4 mkfs.vfat"` for the Rocky Pi
  kernel (6.12.103): 38 MB, zstd, at `/boot/stage0-installer.img`. dracut
  warns "No '/dev/log' or 'logger' included for syslog logging"; harmless.
- **It netboots on a Pi 5** (k52, pilot of 2026-10-27): `kernel8.img` from
  the container's `/boot/efi` plus this initramfs, served by `stage0 serve
  pinet --root`. Copied out with `podman create` and `podman cp` for the
  pilot.

## Adding stage0's files by appending a cpio

- **dracut runs hooks from an appended cpio.** Hooks live in
  `/var/lib/dracut/hooks/<stage>/` (`/usr/lib/dracut/hooks` links there).
  Seen running: `initqueue/online/90-stage0.sh`, once per interface that
  comes up (`$1` is the interface: `end0`, then `lo`), and
  `pre-mount/90-stage0.sh`, after the network is up. Their output to
  `/dev/console` reaches the serial console.
- **The appended archive must start at a 4-byte boundary** when it is an
  uncompressed cpio. The 37,939,845-byte image followed directly by one
  gave "rootfs image is not initramfs (invalid magic at start of compressed
  archive); looks like an initrd", and the hooks never ran; with 3 zero
  bytes between them they did. The kernel skips zeros between archives.
  `stage0 install` pads the same way when it appends its agent to an
  image's initramfs.

## Network in the initramfs

- **`rd.neednet=1`** alone makes dracut's NetworkManager ask DHCP for an
  address (interface `end0` on the Pi 5; `nm-wait-online-initrd` waits
  for it, with no time limit). **Not `ip=dhcp`:** the Rocky Pi kernel has
  its own DHCP client built in (`IP-Config`), which takes `ip=dhcp` first,
  without the vendor class, and retries for minutes before dracut starts.
- **`rd.net.dhcp.vendor-class=stage0-agent`** sets the DHCP vendor class,
  so `stage0 serve` answers it as it answers the agent. Without it, a node
  that netbooted over IPv6 has no IPv4 lease on record, and serve ignores
  the plain request ("not a PXE client and never booted here").
- With no `root=` dracut runs its stages to the end and fails at
  switch-root, then enters emergency mode. The installer initramfs has
  the container's `/etc/passwd` but no `/etc/shadow`; whether its
  emergency shell then lets root in is untested.

## Over IPv6

`install --ipv6` (k52, 2026-10-27): the Pi netbooted by DHCPv6 and TFTP,
dracut's NetworkManager took its SLAAC address from stage0's router
advertisement with only `rd.neednet=1`, and the agent fetched the layout,
the layers and the seed from `[fd00:10:3::1]` and reported there. The
installer's command line leaves out the DHCP vendor class, so stage0's
DHCPv4 never answered it; the first DHCPv4 lease went to the installed
system.

## The installed system

- **The clock:** a Pi has no clock battery. Without help, k52 came up on
  2026-06-10 (systemd's built-in floor) and `rpm` called 89 of 242
  package signatures BAD ("signed in the future"). The agent sets its own
  clock from the `Date:` header of stage0's HTTP server, then sets
  `/usr/lib/clock-epoch` on the installed root to the install time:
  systemd (PID 1) advances a clock that is behind that file's mtime. With
  it, k52 booted about a minute behind pilab, and no signature was BAD
  (2026-10-27). From there chrony keeps it: stage0's DHCP hands out
  pinet's NTP server (chrony on pilab, `ntp:` in `site.yaml`), and Rocky's
  NetworkManager script `20-chrony-dhcp` gives it to chrony
  (`/run/chrony-dhcp/`); k52 synced to `10.0.3.1` after renewing its lease.
