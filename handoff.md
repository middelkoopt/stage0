# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-27)
**Pis:** four disk images install and come up with SSH (Rocky 10.2: Pi 5
only; Ubuntu 26.04: Pi 5 tried), over IPv4 or, with `install --ipv6`, IPv6
alone. **Containers:** `stage0 install k52 --image rocky10-rpi-container`
installs the Rocky container from its OCI layout (45 s; up in 197 s), or
from a registry (`container: <registry>/<repo>:<tag>`); k52 runs it now,
from a local test registry (`10.0.3.100` while serve runs). **VMs:** `stage0 install
vm1 --image rocky-10.2-vm` netboots the agent as a UKI by UEFI HTTP boot
with Ubuntu's firmware (128 s; [`docs/testbed.md`](docs/testbed.md)). Rocky from the
container also runs on k400 (Pi 400).

## Open decisions
(none)

## Next session
1. VMs and containers over IPv6 (agreed next) (`stage0tap0` would need
   `fd00:10:9::1`; dracut's NetworkManager and SLAAC).
2. Later: private registries (credentials in `.secrets`), Ignition,
   x86_64 ([`docs/containers.md`](docs/containers.md)).
3. k53 is powered off; it may be powered and used as root, stopping (not
   disabling) its services. Its `sudo` needs a password stage0 doesn't have.
