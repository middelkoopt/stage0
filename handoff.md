# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/development.md`](docs/development.md)
for code and [`docs/netboot.md`](docs/netboot.md) for Pi work.

## Status (2026-10-25)
The stage0 agent boots on k52 from `stage0 serve`: `stage0 build initrd`
packs the static stage0 binary as `/init`; on the node it prints
`STAGE0-REPORT` (k52: Pi 5, 8 GB, 128 GB SD `mmcblk0`, no NVMe). A 106 MB
initramfs crossed TFTP intact, so the ~92 MB ceiling doesn't apply here
([`docs/netboot.md`](docs/netboot.md)). The disk write (`stage0.disk=` +
`stage0.sha256=` on the command line, xz image as payload) is unit-tested;
its first live run with the 1 GB Rocky 10.2 image is in progress.

## Open decisions
- **K2/K3** How the install reaches the disk. Exploring first: **agent +
  payload** — one large initramfs holding the stage0 agent (PID 1, pre-root)
  and the compressed image, plus a small per-node cpio that the TFTP server
  appends. The agent needs no network: mount, pick the disk, stream-decompress
  the image to it (in Go), write the first-boot seed, reboot. Settled: 106 MB
  crosses TFTP fine, and the Rocky Pi kernel has SD, NVMe and Ethernet
  built in (the agent needs no modules). Open: whether the firmware loads a
  ~1 GB initramfs (the full Rocky image), and the ~6 minutes TFTP takes for
  it. Fallback: (a) small initramfs that fetches the image over the network.

## Next session
1. Finish the install on k52: check the write's console log, stop `serve`,
   power-cycle, watch Rocky boot from the SD card; then first-boot settings
   (cloud-init seed or files) so it comes up with SSH.
2. `stage0 console --send` to type into a node's console (log in, run a
   command) without SSH.
3. Update the Pi EEPROM firmware on the free nodes (allowed); ideally a
   small netbooted updater.
4. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
