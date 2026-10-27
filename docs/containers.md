# Container route facts

Facts for installing from a container (route A,
[`ROADMAP.md`](../ROADMAP.md#install-routes)), as seen in a pilot on k52.
The container is [`containers/rocky10-rpi`](../containers/rocky10-rpi/Containerfile).

> Last verified: 2026-10-27

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

- **`rd.neednet=1 ip=dhcp`** makes dracut's NetworkManager ask for an
  address (interface `end0` on the Pi 5; `nm-wait-online-initrd` waits
  for it, with no time limit).
- **`rd.net.dhcp.vendor-class=stage0-agent`** sets the DHCP vendor class,
  so `stage0 serve` answers it as it answers the agent. Without it, a node
  that netbooted over IPv6 has no IPv4 lease on record, and serve ignores
  the plain request ("not a PXE client and never booted here").
- With no `root=` dracut runs its stages to the end and fails at
  switch-root, then enters emergency mode (root is locked in this
  container, so the shell isn't reachable).
