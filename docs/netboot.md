# Netboot facts

Facts about the platforms and protocols stage0 boots, with sources. Where a
fact comes from the lab notes rather than vendor docs, it says so.

> Last verified: 2026-10-25

## Raspberry Pi 4, 400 and 5

Source unless noted: Raspberry Pi documentation
([`eeprom-bootloader.adoc`](https://github.com/raspberrypi/documentation/blob/develop/documentation/asciidoc/computers/raspberry-pi/eeprom-bootloader.adoc),
[`boot-http.adoc`](https://github.com/raspberrypi/documentation/blob/develop/documentation/asciidoc/computers/raspberry-pi/boot-http.adoc),
[`network-boot-ipv6.adoc`](https://github.com/raspberrypi/documentation/blob/develop/documentation/asciidoc/computers/remote-access/network-boot-ipv6.adoc)).

- **No UEFI.** The EEPROM bootloader loads Pi firmware (`start4.elf`,
  `config.txt`), which loads kernel, initramfs and device tree as separate
  files. A UKI cannot boot a Pi. Community UEFI firmware exists
  ([pftf/RPi4](https://github.com/pftf/RPi4); Pi 5 forks of
  [worproject/rpi5-uefi](https://github.com/worproject/rpi5-uefi)) but adds a
  boot layer.
- **Boot order** is `BOOT_ORDER` in the EEPROM (`rpi-eeprom-config --edit`),
  one nibble per mode, tried right to left: `1` SD, `2` network (TFTP), `4`
  USB, `6` NVMe (Pi 5), `7` HTTP, `f` restart. Example: `0xf21` = SD, then
  network, then loop.
- **TFTP per-node directories:** files are requested under a prefix; if
  `start4.elf` isn't found there, the prefix is dropped. `TFTP_PREFIX`: `0`
  serial number (default), `1` the fixed `TFTP_PREFIX_STR`, `2` MAC address
  (`dc-a6-32-01-36-c2/`). The serial prefix is its last 8 hex digits
  (lab: `piconfig.py`).
- **Static IPv4, no DHCP:** set `TFTP_IP` with `CLIENT_IP`, `SUBNET` and, if
  off-subnet, `GATEWAY`; DHCP is then skipped.
- **IPv4 DHCP** must answer the Pi's PXE request with option 43
  "Raspberry Pi Boot" (dnsmasq: `pxe-service=0,"Raspberry Pi Boot"`).
- **IPv6 netboot** is documented as an experimental alpha for **Pi 4 and CM4
  only**. The Pi sends a router solicitation, configures by SLAAC or DHCPv6 as
  the RA's M flag says, and **always** asks DHCPv6 for the TFTP server
  (`BOOTFILE-URL`, client architecture `0x29`). So IPv6 needs a DHCPv6
  responder even with a static address. The docs say dnsmasq lacks the needed
  parameters; the lab ran it anyway with `dhcp-option=option6:bootfile-url`.
- **HTTP boot** (`BOOT_ORDER` mode `7`, bootloader 2022-03-10 or later)
  downloads `boot.img` (a RAM-disk boot filesystem) and `boot.sig`. Every
  download must be signed; with your own `HTTP_HOST`, your public key must be
  in the EEPROM, and **a public EEPROM update erases it**. Setting `HTTP_HOST`
  means plain HTTP; HTTPS with a custom CA (`HTTP_CACERT_HASH`) is BCM2712
  (Pi 5) only.
- **Lab observations** (`../lab/ReadMe.md`, not in vendor docs): TFTP block
  size had to be capped at 1428 for Pi over IPv6; Pi 5 needed
  `TFTP_FILE_TIMEOUT=60000`; Pi 5 IPv6 netboot failed without manual
  neighbor-table entries on the server.
- **Seen on k52** (Pi 5, bootloader 2025-12-08, IPv4 from warewulf's TFTP,
  2026-10-25 serial log): it asks for `<serial>/config.txt` first and falls
  back to `config.txt` at the root; later files follow paths set in
  `config.txt`. A 49 MB `initramfs8` came over TFTP in about 15 s. The log
  line reads `Boot mode: NETWORK (02) order f41`.
- **Booted by stage0** (k52, same bootloader, `stage0 serve pinet` serving
  k53's Rocky 10.2 boot partition, 2026-10-25): the option 43 bytes in
  `internal/dhcpd` work. The bootloader asked in order: `563cea77/config.txt`
  (missing), `config.txt`, `pieeprom.sig` (missing), the device tree,
  `/config.txt`, `kernel_2712.img` and `armstub8-2712.bin` (missing), then
  `kernel8.img` and `initramfs8`. For the kernel and initramfs it first
  requests the size (tsize) and aborts with "Early terminate", then fetches.
  52 MB took 17 s (3 MB/s). With `console=serial0` the firmware hands the
  kernel `console=ttyAMA10`, the 3-pin debug connector.
- **TFTP size: no ~92 MB ceiling here** (k52, same bootloader, 2026-10-25).
  Every request asks `tsize 0 blksize 1468` (seen with tcpdump). A 106 MB
  initramfs (72,469 blocks, so the 16-bit block number wrapped) arrived
  intact in 34 s: the agent's SHA-256 of its 100 MB random payload matched.
- **The limit is time, not size** (k52, 2026-10-25): with a 1 GB
  initramfs the bootloader gave up after 60.0 s at 183 MB ("Read initramfs8
  failed", then "initramfs disabled") and booted the kernel without it. So a
  file must arrive within the bootloader's per-file TFTP timeout, at ~3
  MB/s here: about 180 MB in 60 s. That `TFTP_FILE_TIMEOUT` (ms, in the
  EEPROM; the lab set 60000 on Pi 5s) is the knob, and that the ~92 MB
  ceiling in `../lab` is the same limit at 30 s, are inferences, unverified.

## IPv6 boot information

- **A router advertisement cannot carry a boot path.** RA options are the
  prefix, MTU, routes, DNS (RFC 8106) and a captive-portal URI (RFC 8910). The
  boot file URL exists only as DHCPv6 option 59
  ([RFC 5970](https://datatracker.ietf.org/doc/html/rfc5970)), so IPv6 netboot
  needs a DHCPv6 responder unless the firmware has the boot URL configured.
