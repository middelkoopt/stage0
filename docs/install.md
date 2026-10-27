# How an install works

How stage0 installs a node on a switch port, as built and run on k52
(Raspberry Pi 5, Rocky 10.2); VMs on the test bed: [`testbed.md`](testbed.md).

```
$ ./stage0 install k52 --image rocky-10.2-rpi     # 4 minutes on k52
```

The binary must be static (`CGO_ENABLED=0 go build ./cmd/stage0`): it is
also the agent. It runs from the repo root (`site.yaml`, `images/`, `logs/`).
Platform facts are in [`netboot.md`](netboot.md), image facts in
[`images.md`](images.md), code layout in [`development.md`](development.md).

> Last verified: 2026-10-27

## What it needs

- **`images/<name>.yaml`:** the image's URL and SHA-256 (an xz raw disk
  image), the `cmdline.txt` the installed system needs, optionally
  `config.txt` lines to ensure (`config:`), files to add to the boot
  partition (`bootFiles:`, e.g. Raspberry Pi OS's `ssh`), its boot
  partition (default 1), the directory in it the firmware boots from
  (`bootDir:`, Ubuntu's `current`), the image's own initramfs for the
  agent to boot on (`initramfs:`, for kernels with drivers as modules),
  and the console pattern that means it is up (default `login: `).
- **`site.yaml`:** the node with a `disk` (optionally a serial `console`
  and a `hostname`, default its name), `sshKeys` (public keys for root on
  installed nodes), and one network with the `vlan` the node's port must be
  on. `install` checks the port's PVID and stops if it is wrong, unless
  `--set-port` is given: then it makes the network's VLAN the port's PVID
  and keeps the old PVID tagged.

## The sequence

`install` does steps 2-9 below; step 1 is set up once.

1. **Network.** The node's switch port has PVID `pinet` (`stage0 set port`);
   the host's `eth0.3` carries `10.0.3.1/24` (systemd-networkd, not stage0).
2. **Stage.** Download the image into `~/.cache/stage0` (once; checked
   against its SHA-256), copy its boot partition's files out (in Go, cached
   by checksum), and build the node's boot root in
   `~/.cache/stage0/run/<node>/`: those files; their `config.txt` plus
   `initramfs stage0-initrd followkernel`; `stage0-initrd` (this binary as
   the agent); `cmdline.txt` with the agent's plan; `seed.cpio` with the
   installed system's `cmdline.txt` (see First-boot settings) and, if the
   image config has `config:` lines, its `config.txt`; the image under
   `images/`. Both `config.txt` files get the image's `config:` lines
   ensured: present as they are, or appended in a stage0 `[all]` section
   (the last value of a setting wins); the agent's also gets `enable_uart=1`,
   since a Pi 4's GPIO serial console is off without it. The Pi firmware picks the
   image's own kernel for the board (`kernel_2712.img` on a Pi 5); both of
   the Rocky image's kernels run the agent.
3. **Serve.** The same servers as `stage0 serve` answer DHCPv4, TFTP and
   HTTP from the boot root on `eth0.3` only, as an ordinary user, logging
   every request.
4. **Netboot.** Powered off and on, the Pi bootloader sends a PXE DHCP
   request; stage0 leases it an address with the Pi's option 43, and the
   bootloader fetches `config.txt`, the kernel and `stage0-initrd` over
   TFTP.
5. **Agent.** `stage0-initrd` (as `stage0 build initrd` makes it) holds the
   static stage0 binary as `/init`. Run as PID 1 it is the agent: it mounts
   `/proc`, `/sys`, `/dev`, waits for disks to settle and prints
   `STAGE0-REPORT {json}` on the console (model, serial, kernel, memory,
   filesystems, disks, interfaces).
6. **Image.** With `stage0.url=` the kernel brings up the network
   (`ip=dhcp`; stage0 answers it because the same MAC just PXE-booted) and
   the agent downloads the image over HTTP into RAM. Without it, the image
   is `/payload` inside the initramfs (`build initrd --payload`, by hand
   only) and the network is not used after the bootloader.
7. **Write.** The agent checks the image's SHA-256 against
   `stage0.sha256=` and only then streams the xz-decompressed image onto
   `/dev/<stage0.disk>`, printing `STAGE0-PROGRESS` lines.
8. **Seed.** With `stage0.seed=`, the agent re-reads the partition table,
   mounts partition 1 (FAT) and unpacks the seed (a cpio from
   `stage0 build seed`) over it, replacing files of the same name. Then it
   prints `STAGE0-INSTALLED {json}` and waits to be powered off.
9. **First boot.** The node is marked installed: from now on stage0
   ignores its PXE requests and answers its system's plain DHCP. Power off
   and on: the bootloader's network attempt gets no answer and it falls
   through to the disk (about 45 s on k52), the installed OS boots, and
   NetworkManager's default connection gets the same address for 24 h.
   `install` waits for the image's `ready` pattern on the console, then for
   that lease.

The agent reports to `install` over HTTP (`stage0.report=`: POSTs of its
report, result or error to `serve`'s `/stage0/report/<kind>`, the one
write `serve` accepts) and on the serial console. With a console,
`install` follows it throughout (`logs/<node>.log`) and waits for the
image's `ready` pattern after the reboot; without one, or if the console
stays quiet (a Pi 4 without `enable_uart=1`), the node is up when its
installed system takes its lease and answers ping and SSH. serve's log
is `logs/<node>-serve.log`. On an agent error `install` powers the node off
and stops. The steps can also be run by hand with `build initrd`, `build
seed`, `serve`, `power` and `console`.

## First-boot settings

The installed system's `cmdline.txt` is the image's `cmdline` plus:

- `systemd.hostname=<hostname>`;
- `systemd.set_credential_binary=ssh.authorized_keys.root:<base64>`: the
  site's `sshKeys`, which systemd's `provision.conf` tmpfiles rule writes
  to `/root/.ssh/authorized_keys` at boot (base64 because keys contain
  spaces). Public keys only: any user can read `/proc/cmdline`.

For images with cloud-init `user-data` on their boot partition (AlmaLinux),
the seed also carries that file with its top-level `hostname:` set to the
node's (otherwise cloud-init sets its own at first boot); the rest of it
stays as shipped.

Nothing is mounted or written inside the installed root filesystem. On k52
(Rocky 10.2) the result logs in as `ssh root@<node>` with a site key and
shows `k52 login:` on the console. The kernel takes at most 2047 bytes of
command line, which bounds the number of keys.

## Reaching the installed node

`install` ends by printing the node's addresses:

```
k52: 10.0.3.100 (IPv4 from stage0: keep `stage0 serve pinet` running to keep it)
  ssh root@10.0.3.100
k52: fe80::a9e7:2c9a:8c3d:d7c9%eth0.3
  ssh root@fe80::a9e7:2c9a:8c3d:d7c9%eth0.3
```

- **IPv4:** the lease the node netbooted with, kept for its installed
  system. Without an answer, NetworkManager's default connection fails
  after about 45 s, takes the interface down (link-local address included)
  and retries every 5 minutes, so an unanswered node is reachable only part
  of the time. The lease lasts 24 h; `stage0 serve pinet` (no `--root`:
  DHCP only) keeps answering installed nodes, from the install records,
  with the same address.
- **IPv6 in the network's `prefix6`** (pinet: `fd00:10:3::/64`), if
  `serve` advertises it: router advertisements with router lifetime 0, so
  nodes form an address (EUI-64 on Alma, stable privacy on Raspberry Pi OS)
  but take no default route. Sending them needs `CAP_NET_RAW`: run `serve`
  and `install` from the copy in `/usr/local/bin` (see development.md);
  without it `serve` logs a warning and runs the rest.
- **IPv6 link-local:** a NetworkManager "stable privacy" address, new with
  every install, found on the wire; `./stage0 get address k52` repeats the
  lookup (an echo to `ff02::1` on the network's interface, then the
  neighbour table, matched against the MACs the agent reported, taking only
  addresses that answer; no root needed).

With the lab key, add `-i ~/.cache/stage0/ssh/id_ed25519` to `ssh`.

With `--ipv6`, the install runs over IPv6 alone (the network needs
`prefix6`, and `install` must run from `/usr/local/bin/stage0` for router
advertisements and neighbour entries): the node netboots by DHCPv6 and
TFTP on IPv6 (its EEPROM needs `USE_IPV6=1`), and the agent takes an
address by SLAAC and fetches from `[<prefix>::1]`; details in
[`netboot.md`](netboot.md#ipv6-netboot-on-a-pi-5). It works for
containers (dracut's NetworkManager takes the SLAAC address; no DHCPv4
class, so stage0's DHCPv4 leaves it alone: [containers](containers.md))
and for VMs (UEFI HTTP boot over IPv6: [test bed](testbed.md)). The
installed system then gets its IPv4 lease too.

With `--keep`, `install` keeps serving after the install (renewing the
node's lease) until interrupted or `--timeout`.

`install` also checks that the IPv4 address answers ping and that its SSH
server greets (`SSH-2.0-…`); `./stage0 check <node>` repeats that check.

`install` keeps a record of each node it installs (the agent's report, the
MAC it netbooted on, its network and IPv4 address) in
`~/.local/state/stage0/nodes/<node>.json` (`$XDG_STATE_HOME`, or
`--state`); `get address` and `serve` read them.

## The agent's command line

The agent acts only on these kernel parameters (in `cmdline.txt` on a Pi).
Without `stage0.disk=` and `stage0.sha256=` it reports and does nothing
else, so booting it never wipes a disk by accident.

| Parameter | Meaning |
| --- | --- |
| `stage0.disk=mmcblk0` | Disk to write, by name under `/dev` |
| `stage0.sha256=<64 hex>` | SHA-256 of the xz image; nothing is written unless it matches |
| `stage0.url=http://…` | Fetch the image from here (needs `ip=dhcp`); otherwise use `/payload` |
| `stage0.seed=http://…` | First-boot files to unpack over partition 1 after writing |
| `stage0.report=http://…` | Also POST the report, result or error to `<url>/<kind>` |

## Limits

- **TFTP time:** the Pi bootloader gives each TFTP file 60 s, about 180 MB
  at the 3 MB/s it gets here, so an image inside `initramfs8` must stay
  under that. Over HTTP the 994 MB Rocky image took 8.5 s.
- **RAM:** the image is held in RAM before it is written (the agent's
  root is the initramfs), so it must fit the node's memory.
- **Seed:** goes to partition 1 only, files and directories only.
- **Leases** live in `serve`'s memory and are lost when it stops.
