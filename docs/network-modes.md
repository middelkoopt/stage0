# Network modes

How stage0 shares a network with other DHCP servers: `mode:` on a network
in `site.yaml`.

> Last verified: 2026-10-27

- **own** (the default): stage0 hands out the addresses from its `pool`,
  answers netboot, and, with `prefix6`, sends router advertisements and
  answers DHCPv6. Everything in the other docs is this mode.
- **proxy** (IPv4): another DHCP server hands out the addresses; stage0
  has no pool and answers boot requests only.

## proxy

Built and unit-tested (`internal/dhcpd`); **not yet verified live**:

- A boot DISCOVER (class `PXEClient` or `HTTPClient`) gets a ProxyDHCP
  OFFER: no address (`yiaddr` 0.0.0.0, no lease time or mask), stage0 as
  server and next server, and the boot options as in own mode: the Pi's
  option 43, or for aarch64 UEFI the boot file (`uki.efi`) or, for HTTP
  boot, its URL with class `HTTPClient`. The client takes its address from
  the other server's offer.
- UEFI PXE then asks the proxy itself on port 4011 (DHCPREQUEST from the
  address it was given); stage0 listens there in proxy mode and ACKs with
  the boot file.
- REQUESTs to the other server, the agent's DHCP and installed systems'
  DHCP get no answer: the other server leases them. Installed nodes are
  still marked, so their netboot goes unanswered.
- `install` can't tell the installed system is up from a lease of its
  own, so it finds the node's IPv6 link-local address by MAC and checks
  ping and SSH there.

Testing: `tools/standin-dhcp.sh start <interface> <address/prefix>
<first,last>` runs dnsmasq as the other server in a network namespace on a
macvlan of the interface (stage0 holds port 67 on the host), handing out
addresses and no boot options; `stop` removes it.

What was seen (2026-10-27): the stand-in on pinet leased k400's installed
Rocky (`10.0.3.86`). But k400's bootloader never asked DHCPv4: with
`USE_IPV6=1` in its EEPROM it tries IPv6 only (about 45 s, then the SD
card), so ProxyDHCP never came into it. A live test needs a Pi with
`USE_IPV6=0`, or a VM on the test bed with the stand-in on its tap.
