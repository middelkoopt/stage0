# Network modes

How stage0 shares a network with other DHCP servers: `mode:` on a network
in `site.yaml`.

> Last verified: 2026-10-27

- **own** (the default): stage0 hands out the addresses from its `pool`,
  answers netboot, and, with `prefix6`, sends router advertisements and
  answers DHCPv6. Everything in the other docs is this mode.
- **proxy**: another server hands out the addresses (DHCPv4, and the
  network's router by SLAAC for IPv6); stage0 has no pool, answers boot
  requests only and sends no router advertisements.
- **none**: no DHCP, DHCPv6 or router advertisements from stage0 at all;
  firmware is set up with the boot server (a Pi's EEPROM `TFTP_IP` and
  static `CLIENT_IP`; a UEFI boot entry), and stage0 serves TFTP and HTTP.

## none

**Works** on k400 (2026-10-28), its EEPROM given `TFTP_IP=10.0.3.1`,
`CLIENT_IP=10.0.3.60`, `SUBNET=255.255.255.0` for the test (removed
again after): the bootloader skipped DHCP (`NET 10.0.3.60 … tftp
10.0.3.1`) and fetched from stage0's TFTP; the agent took its address from
the stand-in DHCP server; up on link-local in 186 s. With no DHCP to
withhold, the reboot first netbooted the installer again: now `install`
closes its TFTP server once the node reports INSTALLED (every request
"not found"), and the bootloader goes on to the SD card. IPv6 netboot is
off in this mode (it starts with DHCPv6).

## proxy

**Works** (2026-10-28) with dnsmasq as the other server, on a VM and on a
Pi:

- vm1 (UEFI HTTP boot): its firmware took `10.0.9.58` from dnsmasq and
  `http://10.0.9.1/uki.efi` from stage0's proxy offer (class
  `HTTPClient`); the agent's DHCP went to dnsmasq too; up on its
  link-local address in 127 s.
- k400 (Pi 400, `USE_IPV6=0`): its bootloader took `10.0.3.86` from
  dnsmasq and option 43 from stage0's proxy offer, then fetched over
  stage0's TFTP; installed, up in 235 s. (It aborted a size probe after
  the first block, which serve now logs as a size probe.)

How it works:

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
<first,last> [<address6/64>]` runs dnsmasq as the other server in a
network namespace on a macvlan of the interface (stage0 holds port 67 on
the host), handing out addresses and no boot options, and with an IPv6
address router advertisements for its /64; `stop` removes it.

### proxy over IPv6

**Works** on k52 (Pi 5, `USE_IPV6=1`, 2026-10-28) with the stand-in also
advertising `fd00:10:3::/64` (SLAAC only): stage0 sent no router
advertisements; the bootloader took its SLAAC address
(`fd00:10:3:0:da3a:ddff:fec5:c1f3`, its MAC's EUI-64) and asked stage0's
DHCPv6 for information only (an Information-Request), which got the
Bootfile-URL and no address; TFTP and the install went over that address;
up on link-local in 184 s. As with its DHCPv6 address in own mode, the
bootloader doesn't answer neighbour solicitations for its SLAAC address
(one try timed out on TFTP), so DHCPv6 adds a permanent neighbour entry
for the prefix's EUI-64 address of the client's MAC (from its DUID).

A Pi with `USE_IPV6=1` in its EEPROM netboots over IPv6 only (k400, about
45 s, then the SD card): it never asks DHCPv4, so IPv4 proxy mode needs
`USE_IPV6=0` (k400 has it since 2026-10-28; k52 keeps `USE_IPV6=1`).
