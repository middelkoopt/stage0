# Development

> Last verified: 2026-10-25

## Build and test

```bash
go build ./cmd/stage0          # binary in the repo root (git-ignored)
go test ./...                  # unit tests; no network or hardware needed
go vet ./... && gofmt -l .     # must be clean
```

Go 1.26 is pinned in `go.mod` (`toolchain go1.26.10`); an older local `go`
downloads it automatically. `go test -race` does not work on the Raspberry Pi
kernel here (ThreadSanitizer needs a 48-bit address space; the Pi kernel has
47), so run race tests on another machine.

## Layout

| Path | What |
| --- | --- |
| `cmd/stage0` | `main`: signal handling, exit code; nothing else |
| `internal/cli` | Commands, verb first (`get ports`, `power on k53`, `set port`, `console`); every command supports `-o json`, write commands `--dry-run` |
| `internal/config` | `site.yaml` (committed: switches, nodes) and `.secrets` (git-ignored, must be mode 600); unknown fields are errors |
| `internal/console` | Serial console capture: copy to screen and log, stop on a pattern (`--until`) |
| `internal/netswitch` | Switch model (ports, PoE, VLANs), port and VLAN lookup, change planning, the `Reader` and `Controller` interfaces |
| `internal/netswitch/gs316ep` | NETGEAR GS316EP driver over its web UI |

## Conventions

- **Tests first.** Hardware behavior is captured as fixtures in `testdata/`
  (redact serials, MACs and session tokens) and replayed by fakes, so
  `go test ./...` never touches the lab.
- **Strict parsing.** Drivers fail on text they don't recognize rather than
  guessing; a firmware change should show up as a test failure.
- **Nodes are named by their switch-port label** (a port number also works).
  Write commands print `before -> after`; `--dry-run` prints the plan and
  writes nothing.
- **Libraries:** cobra (CLI), `go.yaml.in/yaml/v3` (YAML),
  `golang.org/x/net/html` (switch pages), `go.bug.st/serial` (consoles).
  Prefer the standard library otherwise.

## GS316EP notes

- Its only management interface is the web UI. Login posts
  MD5(password interleaved with the page's nonce) and gets a session token
  (`Gambit`) used on every request; the switch allows few sessions, so the
  driver always logs out.
- `.secrets` takes `password:` or `loginHash:` (the value the older
  `../lab/setup/switch.py` stored). This lab currently uses `loginHash:`.
- Port labels on the switch are the machine names, so `stage0 get ports` is
  also the lab inventory.
- **Writes** post the forms the web UI posts; the switch's replies carry
  nothing useful, so each write re-reads the switch in the same session and
  fails if the port didn't end up as planned or any other port's settings
  changed. A change already in effect posts nothing. Verified live on ports
  3-8 (2026-10-25).
- **Field order matters:** the switch silently ignores a form whose fields
  are sorted (as Go's `url.Values` encodes them), so the driver sends them in
  the UI's order with `Gambit` last. `setPvid` takes the port as `PORT`;
  `switch.py`'s `portid` is silently ignored, so its PVID moves don't work.
- **VLAN membership** comes from the VLAN page's own per-VLAN masks
  (`list-vhidmem`: one digit per port, 1 tagged, 2 untagged, 3 not a
  member). A port change edits only that port's digit and posts only VLANs
  whose mask changed, in the order join new PVID's VLAN, `setPvid`, then the
  rest. The port table (PVID plus a VLAN list) can't tell tagged from
  untagged-but-not-PVID (port 9 is untagged in VLAN 2 with PVID 3), so the
  read-back also compares the masks. The page's "VLAN mode" form erases all
  VLAN settings; the driver never sends it.
- **PoE** on/off sends the port's current power limit back unchanged (read
  from the PoE page, in tenths of a watt). Cycle is the UI's "reset", a mask
  with one digit per PoE port (1-15).
