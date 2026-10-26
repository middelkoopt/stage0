# Image facts

Facts about the distro images stage0 installs, as seen installing them.

> Last verified: 2026-10-26

## Rocky Linux 10.2 SBC Raspberry Pi

`Rocky-10-SBC-RaspberryPi-10.2-20260525.0.aarch64.raw.xz` from
`https://dl.rockylinux.org/pub/rocky/10/images/aarch64/` (994 MB; SHA-256
in the `.CHECKSUM` file beside it). Raw size 3,508,535,296 bytes.

- **Layout:** GPT. Partition 1 is the FAT boot partition (from LBA 2048);
  the root filesystem is ext4 on partition 3, PARTUUID
  `ae437f98-7402-4a21-9752-30303d4a7a8a`.
- **It does not boot a Pi 5 as shipped** (k52, written by stage0). The
  firmware picks `kernel_2712.img` (6.6.77, 16k pages), finds no
  `initramfs_2712` ("auto_initramfs disabled"), and the shipped
  `cmdline.txt` (`console=ttyAMA0,115200 console=tty1 root=LABEL=RPIROOT
  rootfstype=ext4 rootwait`) needs an initramfs to resolve `LABEL=`, so the
  kernel panics: "Unable to mount root fs".
- **The shipped console is `ttyAMA0`**, the GPIO-header UART. A probe on
  the Pi 5's debug connector sees nothing after "Starting OS"; `serial0`
  there means the debug connector.
- **With the root named by PARTUUID it boots a Pi 5 without an
  initramfs** (k52, 2026-10-26): `console=tty1 console=serial0,115200
  root=PARTUUID=ae437f98-7402-4a21-9752-30303d4a7a8a rootfstype=ext4
  rootwait` as the seed's `cmdline.txt` reaches "Welcome to Rocky Linux
  10.2" and `localhost login:` on the debug connector.
- **First boot as shipped:** user `rocky`, password `rockylinux` (works on
  the serial console); sshd is installed, enabled and listening on port 22
  (no `/boot/ssh` switch needed; that is Raspberry Pi OS); the root
  filesystem stays 2.3 GB (`/usr/bin/rootfs-expand` and `growpart` are
  there); the wired interface is `end0` (NetworkManager 1.56), DHCP; systemd
  257 with `systemd-ssh-generator`, `systemd-firstboot`, `systemd-repart`.
- **No cloud-init:** no `/usr/bin/cloud-init`, no `/etc/cloud`.
- **`rpm -q` is unreliable on this image:** `rpm -qa` skips 51 packages
  ("Header V4 RSA/SHA256 Signature, key ID 6fedfc85: BAD"), so it reports
  installed packages such as openssh-server as missing. Check files instead.
- k53 runs this release from a boot partition without `kernel_2712.img`, so
  its firmware uses `kernel8.img` with `initramfs8`; that `initramfs8` is
  for 6.6.77 while its `kernel8.img` is 6.12.103 (inferred from the file
  lists; how k53 got that way is unknown).
