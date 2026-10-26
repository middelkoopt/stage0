# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/development.md`](docs/development.md)
for code and [`docs/netboot.md`](docs/netboot.md) for Pi work.

## Status (2026-10-26)
**stage0 installed Rocky 10.2 on k52 (Pi 5) and it boots to a login
prompt.** The agent netboots, gets an address by the kernel's `ip=dhcp`,
fetches the 994 MB image from `serve`'s HTTP (8.5 s), checks its SHA-256,
writes it to the SD card (105 s), and unpacks a seed `cmdline.txt` onto the
boot partition. The shipped image doesn't boot a Pi 5 as is; see
[`docs/images.md`](docs/images.md). k52 is on, at Rocky's login prompt
(user `rocky`, password `rockylinux`, serial only so far). The steps are
run by hand (see Next session 1).

## Open decisions
- **K2/K3** How the install reaches the disk. Built and working: a small
  agent initramfs over TFTP, the image over HTTP. A 1 GB image inside the
  initramfs fails: the bootloader gives each TFTP file a fixed time (60 s
  here, ~180 MB at 3 MB/s); see [`docs/netboot.md`](docs/netboot.md).
  Recommendation: keep the HTTP route and drop the payload-in-initramfs
  route, unless a node can't do `ip=dhcp`. Needs the human's yes.

## Next session
1. `stage0 install <node>`: one command for what was done by hand (stage
   the boot root with a per-node cmdline.txt, serve, power the node, wait
   for `STAGE0-INSTALLED`, stop answering it, power-cycle, wait for
   `login:`), with the per-image settings (`docs/images.md`) in one config
   file per image.
2. First boot that's useful: SSH key, hostname and an address on `piwulf`
   for the new head, and growing the root filesystem. The Rocky image has
   no cloud-init, so these are files in the root filesystem (ext4, which
   the agent's kernel mounts); open: how the seed names its target
   partition.
3. Update the Pi EEPROM firmware on the free nodes (allowed); ideally a
   small netbooted updater.
4. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
