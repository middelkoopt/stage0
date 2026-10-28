# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-28)
**Installs:** four disk images on the Pis; the Rocky container from
podman's store, a (private) registry or a saved layout, partitioned by
the agent or Ignition; VMs by UEFI HTTP boot of a UKI. **First boot:**
stage0, Ignition or cloud-init, verified on Pis, containers and VMs
([`docs/firstboot.md`](docs/firstboot.md)). **Networks:** own, proxy
(beside another DHCP server and router) and none (firmware knows the
boot server), IPv4 and IPv6 ([`docs/network-modes.md`](docs/network-modes.md)).
**BMC:** `stage0 bmc` answers IPMI for the Pis: power by PoE, SOL by the
serial console. Nothing is pushed.

## Open decisions
- **BK2:** make stage0 a lab service (one long-running `stage0 serve`
  that installs on request) rather than one process per install? —
  recommend yes, after step 3; it decides whether installs share serve.
- **BK3:** a VM-capable container for CI (QEMU with KVM or TCG inside
  podman)? — recommend yes, TCG fallback; it decides how CI runs vm1.

## Next session
1. Refactor (BK1), step 3: committed test scenarios (`tests/sites/*.yaml`,
   image variants) and `tools/lab-matrix.sh` to run them; image-config
   kinds (disk image, container, VM) so each route checks its own fields.
   Steps 1 and 2 are done (up rule, stale-copy check, plain logs;
   `internal/bootplan`, `initrd.Append`, `install()` split).
2. Steps 4 and 5 wait on BK3 and BK2.
3. Nodes: k52 runs the Rocky container; k400 Raspberry Pi OS, EEPROM
   `USE_IPV6=0` (no static settings); vm1 stopped. Nothing serves DHCP on
   pinet now. k53 is powered off; it may be powered and used as root,
   stopping (not disabling) its services. Its `sudo` needs a password
   stage0 doesn't have.
