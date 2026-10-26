# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/development.md`](docs/development.md)
for code and [`docs/netboot.md`](docs/netboot.md) for Pi work.

## Status (2026-10-25)
`stage0 serve pinet --root DIR` (DHCPv4 for PXE clients + read-only TFTP,
as an ordinary user, pinned to eth0.3 at 10.0.3.1) booted k52 into Rocky's
kernel and initramfs from k53's boot files; k52's own disk then failed to
switch root (emergency shell). Built and tested, not committed. k52's port is
now PVID pinet(3); pilab's eth0.3 carries 10.0.3.1/24 (systemd-networkd).

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

## Next session
1. Agent + payload on k52: a stage0 agent as PID 1 in a large initramfs
   (appended to the Rocky initramfs, or alone), first just reporting the
   disks over the console; then the image write. The large-initramfs test
   also answers the ~92 MB TFTP question (the TFTP library wraps its 16-bit
   block counter rather than failing; whether the Pi accepts that is open).
2. Update the Pi EEPROM firmware on the free nodes once they boot (allowed);
   ideally a small netbooted updater.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root to watch the network, stopping (not disabling) its services.
   Its `sudo` needs a password stage0 doesn't have.
