# Virtual BMC

`stage0 bmc` answers IPMI for nodes that have no BMC of their own (the
Pis), so stage-1 tools can drive them like servers: chassis power switches
the node's PoE port, Serial-over-LAN bridges its serial console. Code: `ipmi` (the protocol),
`internal/cli/bmc.go` (nodes, switch).

> Last verified: 2026-10-27

## Setup

- `site.yaml`: a node gets `bmc: {port: <UDP port>}`; each node is its own
  endpoint. Here the port is 623 plus the node's switch port: k52 6231,
  k400 6238.
- `.secrets`: the login, `bmc: {user: …, password: …}` (password at most
  20 bytes, IPMI 2.0's limit), the same for every node.
- `stage0 bmc [node…]` serves them until interrupted, each on stage0's
  address on the node's network (`10.0.3.1` on pinet) unless `--listen`
  gives another. It opens the switch once and changes one port at a time.

```
ipmitool -I lanplus -H 10.0.3.1 -p 6238 -U admin -E chassis power status
```

(`-E` reads the password from `IPMI_PASSWORD`, keeping it off the command
line.)

## What it speaks

- **IPMI 2.0 over the LAN (RMCP+)**, as `ipmitool -I lanplus` does: Get
  Channel Authentication Capabilities and Get Channel Cipher Suites before
  a session; Open Session, RAKP 1–4; then signed and encrypted messages.
- **Cipher suites 3 and 17**: RAKP-HMAC-SHA1 / HMAC-SHA1-96 / AES-CBC-128,
  and the same with SHA-256 (ipmitool 1.8.19's default). The session keys
  K1 and K2 are HMAC(SIK) of 20 bytes of 0x01 and 0x02 for SHA-256 too:
  ipmitool derives them so, and 32-byte constants failed its first signed
  message.
- **Commands:** Get Device ID, Set Session Privilege Level, Close Session,
  Get Chassis Status (power from the PoE port's state), Chassis Control
  (off, on, cycle; hard reset is a cycle, soft off is off: a Pi has no
  ACPI).
- **Chassis control is accepted at once and done in the background**, in
  order, one at a time per node, as a BMC does: a PoE change through the
  GS316EP's web UI takes seconds, a cycle about 6 s, longer than ipmitool
  waits, and it resends. A resent request (the same decrypted bytes within
  30 s) is answered from the session's cache without running again; before
  that, one `power on` ran four times. Status shows the switch's state, so
  it can lag a command by seconds.

- **SOL** (`ipmitool sol activate`): Get SOL Configuration Parameters
  (enabled, 115200 baud, channel 1, this UDP port), Activate and
  Deactivate Payload, Get Payload Activation Status; one SOL session per
  node. Activating opens the node's serial console (its `console:` in
  `site.yaml`) and deactivating closes it, so `stage0 console` and SOL
  can't hold the port at once. Output goes in packets of up to 200
  characters, one in flight, resent every 500 ms up to 7 times until
  acknowledged; typed characters are written to the console and
  acknowledged.

Verified with ipmitool 1.8.19 on pilab (2026-10-27): power status, off, on
and cycle on k400, which came back up; `mc info`; a wrong password is
refused; SOL on k52 showed `k52 login:`, took `root` and gave a root shell
(k400 printed nothing: it was installed before its image config had
`enable_uart=1`). `go test ./ipmi` runs the same with ipmitool
against a fake node and console (skipped where ipmitool isn't installed);
the race detector can't run on pilab's kernel ("unsupported VMA range").

Not yet: more than one user, IPv6.
