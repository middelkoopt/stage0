# Stage0 Handoff
Stage0 installs a head node from this repo alone. Read
[`ROADMAP.md`](ROADMAP.md) first, then [`docs/install.md`](docs/install.md)
for how an install works and [`docs/development.md`](docs/development.md)
for code.

## Status (2026-10-26)
**`./stage0 install k52 --image rocky-10.2-rpi` installs Rocky 10.2 on k52
(Pi 5) and waits for its login prompt: 4 minutes, one command**, from the
image alone ([`docs/install.md`](docs/install.md)). The installed system
has the image's defaults: user `rocky`, password `rockylinux`, sshd on, no
key, hostname `localhost`. k52 is on, at that prompt. Uncommitted.

## Open decisions
(none)

## Next session
1. First-boot settings ([`ROADMAP.md`](ROADMAP.md#next) item 1): try
   `systemd.hostname=` and the SSH key credential on `cmdline.txt` (from
   site.yaml) on k52; if the key doesn't take, the seed's `root/`
   directory.
2. Try the same install on k400 (Pi 400, port 8, now on piwulf): the
   firmware should pick `kernel8.img`.
3. k53 is powered off (it may run DHCP on piwulf); it may be powered and
   used as root, stopping (not disabling) its services. Its `sudo` needs a
   password stage0 doesn't have.
