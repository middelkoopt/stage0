# Stage0

Stage0 installs one head node — a base OS on disk, reachable over SSH — from
nothing but this repo and a connection to the provisioning network, so stage-1
provisioners can be CI-tested on real and virtual hardware. Raspberry Pi 5
first. Working, by hand: it installs Rocky 10.2 on a Pi 5 over the network
(`stage0 install`; see [`docs/install.md`](docs/install.md)). A second
binary, `labctl`, drives the lab: switch ports and VLANs, PoE power, serial
consoles and BMCs for the Pis (`labctl power`, `labctl set port`,
`labctl console`, `labctl bmc`).

- [`ROADMAP.md`](ROADMAP.md) — the plan and the intended design.
- [`handoff.md`](handoff.md) — where work stands now, and open decisions.
- [`docs/index.md`](docs/index.md) — how things work and verified facts.
- [`AGENTS.md`](AGENTS.md) — rules for AI sessions in this repo.
