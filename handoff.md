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
- BB1: partition layouts from the node (site.yaml) or image config,
  falling back to the container's labels, served to the agent as JSON —
  recommended; it decides where layouts live.
- BB2: time sync in the container (k52 boots with June's clock; rpm calls
  signatures bad) — recommended: install chrony in the Containerfile;
  turns on whether pinet reaches an NTP server.

## Next session
1. BB1, then containers over `--ipv6`, a registry, Ignition for
   partitioning ([`docs/containers.md`](docs/containers.md)).
2. UEFI netboot in QEMU with Ubuntu's firmware
   ([`docs/netboot.md`](docs/netboot.md#qemu-aarch64)); bootc (E) later.
3. Not done: VMs over IPv6 (`stage0tap0` would need `fd00:10:9::1`).
4. k53 is powered off; it may be powered and used as root, stopping (not
   disabling) its services. Its `sudo` needs a password stage0 doesn't have.
