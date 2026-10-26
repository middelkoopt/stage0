# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**stage0 installed Rocky 10.2 on k52 (Pi 5) and it boots to a login
prompt.** The agent netboots, gets an address by the kernel's `ip=dhcp`,
fetches the 994 MB image from `serve`'s HTTP (8.5 s), checks its SHA-256,
writes it to the SD card (105 s), and unpacks a seed `cmdline.txt` onto the
boot partition. The shipped image doesn't boot a Pi 5 as is; see
[`docs/images.md`](docs/images.md). k52 is on, at Rocky's login prompt
(user `rocky`, password `rockylinux`, serial only so far). The steps are
run by hand: [`docs/install.md`](docs/install.md).

## Open decisions
- **JJ1** How stage0 joins the QEMU test bed's virtual switch: a user-owned
  tap device (one-time sudo; `serve` runs unchanged, same code as on
  hardware) or gVisor's netstack (no root; big dependency, different stack).
  Recommendation: tap. Turns on whether CI machines without root matter.

## Next session
1. `stage0 install <node>` and per-image config files: [`ROADMAP.md`](ROADMAP.md#next)
   items 1-2. Then first-boot settings (item 3), trying the systemd
   credentials on `cmdline.txt` first.
2. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
