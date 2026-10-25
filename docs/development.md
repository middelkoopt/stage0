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
| `internal/cli` | Commands, verb first (`get ports`); every command supports `-o json` |
| `internal/config` | `site.yaml` (committed) and `.secrets` (git-ignored, must be mode 600); unknown fields are errors |
| `internal/netswitch` | Switch model (ports, PoE, VLANs) and the `Reader` interface |
| `internal/netswitch/gs316ep` | NETGEAR GS316EP driver over its web UI |

## Conventions

- **Tests first.** Hardware behavior is captured as fixtures in `testdata/`
  (redact serials, MACs and session tokens) and replayed by fakes, so
  `go test ./...` never touches the lab.
- **Strict parsing.** Drivers fail on text they don't recognize rather than
  guessing; a firmware change should show up as a test failure.
- **Libraries:** cobra (CLI), `go.yaml.in/yaml/v3` (YAML),
  `golang.org/x/net/html` (switch pages). Prefer the standard library
  otherwise.

## GS316EP notes

- Its only management interface is the web UI. Login posts
  MD5(password interleaved with the page's nonce) and gets a session token
  (`Gambit`) used on every request; the switch allows few sessions, so the
  driver always logs out.
- `.secrets` takes `password:` or `loginHash:` (the value the older
  `../lab/setup/switch.py` stored). This lab currently uses `loginHash:`.
- Port labels on the switch are the machine names, so `stage0 get ports` is
  also the lab inventory.
