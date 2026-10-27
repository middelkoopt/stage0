# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-27)
**Pis:** four images install and come up with SSH (Rocky 10.2: Pi 5 only;
Ubuntu 26.04: Pi 5 tried), over IPv4 or, with `install --ipv6`, IPv6
alone. k52 runs Ubuntu 26.04 (link-local only: nothing serves DHCP; its
`ubuntu` user has a throwaway password set on the console), k400 Alma.
**VMs:** `stage0 install vm1 --image rocky-10.2-vm` installs in QEMU
([`docs/testbed.md`](docs/testbed.md)). **Containers:**
`containers/rocky10-rpi` builds with podman on pilab (installer initramfs
inside), netboots on k52, and runs a hook appended to its initramfs;
stage0 can't install from it yet. k52 sits in that pilot's emergency
shell (Ubuntu still on its card).

## Open decisions
(none)

## Next session
1. Container route A prototype ([`ROADMAP.md`](ROADMAP.md#install-routes)):
   `podman save --format oci-dir localhost/stage0/rocky10-rpi`, read the
   layout in Go, take kernel and installer initramfs from the layers,
   append agent plus dracut hook (proven by hand on k52:
   [`docs/containers.md`](docs/containers.md)); then the agent's
   partition-and-unpack step. Ignition for partitioning is a candidate.
2. UEFI netboot in QEMU with Ubuntu's firmware
   ([`docs/netboot.md`](docs/netboot.md#qemu-aarch64)); bootc (E) later.
3. Not done: VMs over IPv6 (`stage0tap0` would need `fd00:10:9::1`).
4. k53 is powered off; it may be powered and used as root, stopping (not
   disabling) its services. Its `sudo` needs a password stage0 doesn't have.
