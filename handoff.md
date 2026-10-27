# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-27)
**Pis:** four disk images install and come up with SSH (Rocky 10.2: Pi 5
only; Ubuntu 26.04: Pi 5 tried). **Containers:** the Rocky container
installs on the Pi 5 and Pi 400 from a saved layout or a registry
(`container:`). **VMs:** `stage0 install vm1 --image rocky-10.2-vm`
netboots the agent as a UKI by UEFI HTTP boot (128 s;
[`docs/testbed.md`](docs/testbed.md)). **IPv6:** `install --ipv6` runs
over IPv6 alone for disk images, containers and VMs. k52 runs the Rocky
container (installed over IPv6), k400 Rocky from the container.

## Open decisions
(none)

## Next session
1. Choose the next arc ([`ROADMAP.md`](ROADMAP.md#milestones-order-tentative)):
   milestone 4 (network modes), the rest of 5 (x86_64, IPMI), 6
   (install-once state for stage 1), or private registries and Ignition
   for containers.
2. Small: an IPv6-only VM install waits ~140 s for the firmware's IPv4 HTTP
   boot to time out.
3. k53 is powered off; it may be powered and used as root, stopping (not
   disabling) its services. Its `sudo` needs a password stage0 doesn't have.
