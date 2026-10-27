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
| `cmd/stage0` | `main`: signal handling, exit code; runs the agent instead when it is PID 1 |
| `internal/cli` | Commands, verb first (`get ports`, `power on k53`, `set port`, `console`, `serve`, `build initrd`, `build seed`, `install`, `get address`); every command supports `-o json`, write commands `--dry-run` |
| `internal/config` | `site.yaml` (committed: switches, nodes with console and install disk, networks with their VLAN) and `.secrets` (git-ignored, must be mode 600); unknown fields are errors |
| `internal/dhcpd` | DHCPv4 for network-booting nodes: answers PXE clients, then the same MACs' kernels (`ip=dhcp`, no vendor class) with the same address; installed nodes get plain leases (24 h) and no answer to PXE; in-memory leases; the Pi's option 43 |
| `internal/httpd` | HTTP (port 80) from the same root, every request logged; for images too big or slow for TFTP. Read-only except agents' `POST /stage0/report/<kind>`, when a listener is set |
| `internal/radv` | IPv6 router advertisements of one prefix (lifetime 0, no default route), for SLAAC; needs `CAP_NET_RAW` |
| `internal/reach` | Ping (unprivileged ICMP) and the SSH server's greeting |
| `internal/tftpd` | Read-only TFTP from one directory (confined with `os.Root`), every request logged |
| `internal/agent` | stage0 on the node (PID 1 of its initramfs): prints `STAGE0-REPORT {json}` (disks, interfaces, payload SHA-256); writes an xz image to a disk only when the kernel command line has `stage0.disk=` and a matching `stage0.sha256=`; fetches it first from `stage0.url=` (HTTP, with `ip=dhcp`) or uses `/payload`; then unpacks `stage0.seed=` (a cpio of first-boot files) over the image's first (FAT) partition |
| `internal/initrd` | Builds that initramfs: the static stage0 binary as `/init`, `/dev/console`, optional `/payload`; and seeds (`build seed`) |
| `internal/image` | Image config files (`images/<name>.yaml`), download and SHA-256 check into the cache, boot files out of the image's boot partition |
| `internal/ptable` | GPT and MBR partition tables of a disk image |
| `internal/fat` | Read-only FAT12/16/32 as an `io/fs.FS`, long names included |
| `internal/neigh` | Finds a MAC's IPv6 addresses on an interface: echo to `ff02::1`, then the netlink neighbour table |
| `internal/vswitch` | Virtual Ethernet switch for the QEMU test bed: QEMU joins over a Unix socket (`-netdev stream`, 4-byte big-endian length per frame); learns MACs, floods the rest. Not yet used by a command |
| `internal/cpio` | newc cpio reader and writer (the initramfs format), no root or host tools |
| `internal/console` | Serial console capture: copy to screen and log, stop on a pattern (`--until`) |
| `internal/netswitch` | Switch model (ports, PoE, VLANs), port and VLAN lookup, change planning, the `Reader` and `Controller` interfaces |
| `internal/netswitch/gs316ep` | NETGEAR GS316EP driver over its web UI |

## Conventions

- **Tests first.** Hardware behavior is captured as fixtures in `testdata/`
  (redact serials, MACs and session tokens) and replayed by fakes, so
  `go test ./...` never touches the lab. Disk and FAT fixtures come from
  `tools/make-fixtures.sh`. Checks against real images run only when
  `STAGE0_TEST_IMAGE` (raw), `STAGE0_TEST_IMAGE_XZ` or
  `STAGE0_TEST_INITRAMFS` names one.
- **Strict parsing.** Drivers fail on text they don't recognize rather than
  guessing; a firmware change should show up as a test failure.
- **Nodes are named by their switch-port label** (a port number also works).
  Write commands print `before -> after`; `--dry-run` prints the plan and
  writes nothing.
- **Libraries:** cobra (CLI), `go.yaml.in/yaml/v3` (YAML),
  `golang.org/x/net/html` (switch pages), `go.bug.st/serial` (consoles),
  `insomniacslk/dhcp` (DHCPv4 packets only; the socket is stage0's),
  `pin/tftp/v3` (TFTP), `ulikunitz/xz` (image decompression on the node).
  Prefer the standard library otherwise.
- **The agent is the same binary.** Build it static (`CGO_ENABLED=0 go build
  ./cmd/stage0`); `stage0 build initrd` refuses a dynamically linked one.
- **`serve` runs as an ordinary user, on one interface.** Its sockets are
  pinned with `SO_BINDTODEVICE` (allowed unprivileged on this kernel), and it
  refuses to start unless the network's interface is up and is the only one
  carrying the network's address. Ports 67 and 69 bind unprivileged here
  because `/etc/sysctl.d/50-unprivileged-ports.conf` sets
  `net.ipv4.ip_unprivileged_port_start=0`; elsewhere, give the binary
  `cap_net_bind_service` (lost on each rebuild). DHCP replies are broadcast,
  so no raw sockets are needed. The host's address on the network is the
  host's own config (systemd-networkd here), not stage0's.
- **`CAP_NET_RAW` for router advertisements:** `/usr/local/bin/stage0` is a
  copy with the capability; after a rebuild, refresh it with
  `sudo install -m 0755 stage0 /usr/local/bin/stage0` and
  `sudo setcap cap_net_raw=ep /usr/local/bin/stage0` (allowed in this repo's
  `.claude/settings.local.json`; the global `ask` rule on `sudo` still
  prompts). Run `serve` and `install` from it to advertise `prefix6`.
- **The test bed's tap device** `stage0tap0` (owned by mtim, `10.0.9.1/24`,
  connected to nothing real) comes from
  `/etc/systemd/network/stage0tap0.netdev` and `.network`.
- **Packet capture without sudo:** `/usr/bin/tcpdump` has
  `cap_net_raw,cap_net_admin`, so plain `tcpdump` works for every user, e.g.
  `tcpdump -i eth0.3 -n port 67 or port 69`; `sudo tcpdump` always prompts.
  An apt upgrade of tcpdump drops the capabilities; reapply with
  `sudo setcap cap_net_raw,cap_net_admin=eip /usr/bin/tcpdump`.

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
