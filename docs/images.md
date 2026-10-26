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
- **SSH keys from the kernel command line work:** systemd's
  `/usr/lib/tmpfiles.d/provision.conf` is present with
  `f^ /root/.ssh/authorized_keys … ssh.authorized_keys.root`, and root
  logs in with a key over SSH (2026-10-26, installed by stage0).
- **`cmdline.txt` is cut short** (k52, 2026-10-26): of a 1,068-byte
  `cmdline.txt`, the kernel got the firmware's 265 bytes plus only the
  first 864 of ours (`/proc/cmdline` 1,130 bytes), so a base64 credential
  at the end arrived broken ("Failed to decode binary credential … Broken
  pipe") and systemd dropped it. One ed25519 key (about 350 bytes in all)
  works; an RSA 3072 key doesn't fit. The kernel allows 2,047; where the
  cut happens (firmware?) is unverified. stage0 refuses more than 850.
- **On a Pi 4 / Pi 400 the GPIO serial console is silent** without
  `enable_uart=1`, which the image's `config.txt` lacks (the mini UART is
  off by default; a Pi 5's debug connector is a separate, always-on UART).
  On k400 the first agent boot without it printed nothing after the
  firmware and never fetched the image (its first console write probably
  blocked; inferred). `images/rocky-10.2-rpi.yaml` ensures it.
- **It doesn't boot a Pi 400 from its SD card** (k400, 2026-10-26, three
  installs): the EEPROM bootloader reads `config.txt` and starts
  `start4.elf` from partition 1, but `start4.elf` then can't read the
  partition at all (with `uart_2ndstage=1`: "Failed to load Device Tree
  file", "Failed to open command line file 'cmdline.txt'", "No compatible
  kernel found"), including files stage0 never touches. Over TFTP the same
  `start4.elf` and `kernel8.img` run the agent fine. Unverified guess: the
  boot partition's GPT type (EFI System; Alma's image uses Microsoft basic
  data).
- **On a Pi 400 the firmware picks `kernel8.img`**, which runs the agent
  over TFTP (k400, 2026-10-26); writing the image to its SD card took 378 s
  (k52's: 101 s).
- **No cloud-init:** no `/usr/bin/cloud-init`, no `/etc/cloud`.
- **`rpm -q` is unreliable on this image:** `rpm -qa` skips 51 packages
  ("Header V4 RSA/SHA256 Signature, key ID 6fedfc85: BAD"), so it reports
  installed packages such as openssh-server as missing. Check files instead.
- k53 runs this release from a boot partition without `kernel_2712.img`, so
  its firmware uses `kernel8.img` with `initramfs8`; that `initramfs8` is
  for 6.6.77 while its `kernel8.img` is 6.12.103 (inferred from the file
  lists; how k53 got that way is unknown).

## AlmaLinux 10.1 Raspberry Pi

`AlmaLinux-10-RaspberryPi-gpt-10.1-20260520.aarch64.raw.xz` from
`https://repo.almalinux.org/almalinux/10/raspberrypi/images/` (323 MB;
SHA-256 in `CHECKSUM` there). Raw size 3,040,870,400 bytes.

- **Installs and boots on a Pi 400 and a Pi 5** (k400, k52, 2026-10-26):
  written in about 160 s, sshd answering, root logs in with the site's
  keys (the kernel-command-line credential works here too). On the Pi 5
  the firmware runs `kernel8.img` (there is no `kernel_2712.img`).

- **Layout:** GPT. Partition 1 (from LBA 8192, type "Microsoft basic
  data", FAT, label `CIDATA`) is the boot partition; ext4 root on
  partition 2, PARTUUID `3939c54e-e3fe-45cd-9c75-cf4ba65823d2`.
- **Boot files:** `kernel8.img` + `initramfs8` (6.12.47, v8), no
  `kernel_2712.img`. `config.txt` sets `auto_initramfs=1` and
  `enable_uart=1`; `cmdline.txt` names the root by PARTUUID.
- **cloud-init NoCloud** from the boot partition (`user-data`,
  `meta-data`): the shipped `user-data` sets hostname `almalinux.local`,
  creates user `almalinux` (sudo, a password hash, no SSH key) and turns
  SSH password login off. Its README: add an SSH key to `user-data` before
  first boot. Left as shipped, cloud-init overrides stage0's
  `systemd.hostname=` with `almalinux.local`; stage0 seeds `user-data` with
  the node's hostname instead.

## Raspberry Pi OS Lite (Debian 13 "trixie"), 2026-10-06

`2026-10-06-raspios-trixie-arm64-lite.img.xz` from
`https://downloads.raspberrypi.com/raspios_lite_arm64/images/` (550 MB;
`.sha256` beside it).

- **Layout:** MBR. Partition 1 (from 8 MiB, FAT, label `bootfs`) is the
  boot partition; root on partition 2, PARTUUID `27b94958-02`.
- **Boot files:** `kernel_2712.img` + `initramfs_2712` (Pi 5) and
  `kernel8.img` + `initramfs8`; `config.txt` sets `auto_initramfs=1`;
  `cmdline.txt` names the root by PARTUUID and asks for `resize`, which
  grows the root filesystem at first boot.
- **cloud-init NoCloud** from the boot partition (`user-data`,
  `meta-data`, `network-config`); the shipped `user-data` is all comments
  (no user, no hostname).
- **SSH is off unless the boot partition has a file named `ssh`**; the
  image config adds an empty one (`bootFiles`).
- **Installs and boots on a Pi 5 and a Pi 400** (k52, k400,
  2026-10-26). On k400 without `enable_uart=1` the system came up but its
  console stayed silent; the image config now ensures it. On k52: written
  in 97 s, up in 4 minutes with hostname `k52` (seeded `user-data`), root logging in
  with the site's keys (the kernel-command-line credential), sshd
  `OpenSSH_10.0p2 Debian-7+deb13u4`, root filesystem grown to the disk
  (116.9 GB).
