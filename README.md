# Stage0

Stage0 installs one head node — a base OS on disk, reachable over SSH — from
nothing but this repo and a connection to the provisioning network, so stage-1
provisioners can be CI-tested on real and virtual hardware. Raspberry Pi 5
first. Early: it drives the lab switch (`stage0 get ports`, `stage0 power
on|off|cycle <node>`, `stage0 set port <node>`) and captures serial consoles
(`stage0 console <node>`); see [`docs/development.md`](docs/development.md).

- [`ROADMAP.md`](ROADMAP.md) — the plan and the intended design.
- [`handoff.md`](handoff.md) — where work stands now, and open decisions.
- [`docs/index.md`](docs/index.md) — how things work and verified facts.
- [`AGENTS.md`](AGENTS.md) — rules for AI sessions in this repo.
