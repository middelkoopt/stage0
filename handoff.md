# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**`./stage0 install k52 --image rocky-10.2-rpi` installs Rocky 10.2 on k52
(Pi 5) in 4 minutes and leaves it reachable as `ssh root@10.0.3.100`**
(site keys, hostname `k52`; IPv6 link-local too): stage0 keeps the node's
lease for its installed system, so NetworkManager's connection stays up
([`docs/install.md`](docs/install.md#reaching-the-installed-node)). The
lease lasts 24 h; `stage0 serve pinet` renews it. Uncommitted.

## Open decisions
(none)

## Next session
1. Try the same install on k400 (Pi 400, port 8, now on piwulf; move it to
   pinet first): the firmware should pick `kernel8.img`.
2. Then the roadmap's next items ([`ROADMAP.md`](ROADMAP.md#next)): "node
   reports up", moving ports, more images. IPv4 for installed nodes (OO1),
   growing the root filesystem (OO2) and the seed's `root/` (GG2) are
   deferred there.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
