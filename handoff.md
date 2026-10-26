# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/development.md`](docs/development.md)
for code and [`docs/netboot.md`](docs/netboot.md) for Pi work.

## Status (2026-10-25)
Second slice committed and verified live: `stage0 power`, `stage0 set port`,
`stage0 console`, `site.yaml` nodes with consoles (k52 on the Debug Probe,
k400 on the CP2102). Switch quirks are in
[`docs/development.md`](docs/development.md#gs316ep-notes).

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
1. DHCPv4 + TFTP on `pinet` (VLAN 3) with transfer logging, and the agent +
   payload experiment on **k52** (Pi 5, port 1, console on the Debug Probe;
   free to wipe). k52 now netboots warewulf's image from k53 on `piwulf` and
   hangs at `ignition-fetch` (so does k400); moving its port to `pinet` takes
   it off warewulf. k53 is the working warewulf head: use it from the inside,
   don't power it.
