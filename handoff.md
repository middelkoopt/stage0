# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/development.md`](docs/development.md)
for code and [`docs/netboot.md`](docs/netboot.md) for Pi work.

## Status (2026-10-25)
First slice built, tested and committed: Go module, CLI (`stage0 get
ports|vlans`, `-o json`), `site.yaml` + `.secrets`, read-only GS316EP driver;
works against the lab switch. First image is Rocky 10 (checked: no UEFI shim;
its stock initramfs has no networking).

## Open decisions
- **K2/K3** How the install reaches the disk. Exploring first: **agent +
  payload** — one large initramfs holding the stage0 agent (PID 1, pre-root)
  and the compressed image, plus a small per-node cpio that the TFTP server
  appends. The agent needs no network: mount, pick the disk, stream-decompress
  the image to it (in Go), write the first-boot seed, reboot. Unknowns: the
  Pi 5 TFTP/initramfs size ceiling (~92 MB reported; 65,535 TFTP blocks ×
  1,428 B = 93.6 MB suggests block-number wrap — unverified), RAM headroom,
  and whether the Rocky Pi kernel has SD/NVMe drivers built in or needs
  modules from the image. Fallback: (a) small initramfs that fetches the
  image over the network.
- **V1** Verbs for the next slice — recommend `stage0 power on|off|cycle
  <node>`, `stage0 set port <node> --pvid pinet --tagged piwulf,coffee`,
  `stage0 console <node>` (`<node>` = switch port label); sets the command
  shape later slices build on.
- **I2** Which node each serial adapter is wired to (Debug Probe
  `/dev/ttyACM0`, CP2102 `/dev/ttyUSB0`).

## Next session
1. Next slice: PoE power on/off/cycle and VLAN moves (write side of the
   switch driver, against captured POST fixtures), serial console capture.
2. Then DHCPv4 + TFTP on `pinet` (VLAN 3) with transfer logging, and the
   agent + payload experiment on k53 (switch port 2).
