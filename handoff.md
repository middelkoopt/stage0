# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-27)
**Pis:** four disk images install and come up with SSH (Rocky 10.2: Pi 5
only; Ubuntu 26.04: Pi 5 tried), over IPv4 or, with `install --ipv6`, IPv6
alone. **Containers:** `stage0 install k52 --image rocky10-rpi-container`
installs the Rocky container from its OCI layout (45 s; up in 197 s);
k52 runs it now (`10.0.3.100` while serve runs). **VMs:** `stage0 install
vm1 --image rocky-10.2-vm` ([`docs/testbed.md`](docs/testbed.md)).

## Open decisions
- BC1: an NTP server for pinet (the installed Pis' chrony has none) —
  recommended: chrony on pilab serving pinet, handed out by stage0's
  DHCP (option 42); turns on whether pilab may run it.

## Next session
1. BB1 (agreed): partition layouts from the node (site.yaml), then the
   image config, then the container's labels, served to the agent as
   JSON; the installer kernel/initramfs the same way. Then containers over
   `--ipv6`, a registry, Ignition ([`docs/containers.md`](docs/containers.md)).
2. UEFI netboot in QEMU with Ubuntu's firmware
   ([`docs/netboot.md`](docs/netboot.md#qemu-aarch64)); bootc (E) later.
3. Not done: VMs over IPv6 (`stage0tap0` would need `fd00:10:9::1`).
4. k53 is powered off; it may be powered and used as root, stopping (not
   disabling) its services. Its `sudo` needs a password stage0 doesn't have.
