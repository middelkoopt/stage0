# How an install works

How stage0 installs a node, as built and run on k52 (Raspberry Pi 5,
Rocky 10.2):

```
$ ./stage0 install k52 --image rocky-10.2-rpi     # 4 minutes on k52
```

The binary must be static (`CGO_ENABLED=0 go build ./cmd/stage0`): it is
also the agent. It runs from the repo root (`site.yaml`, `images/`, `logs/`).
Platform facts are in [`netboot.md`](netboot.md), image facts in
[`images.md`](images.md), code layout in [`development.md`](development.md).

> Last verified: 2026-10-26

## What it needs

- **`images/<name>.yaml`:** the image's URL and SHA-256 (an xz raw disk
  image), the `cmdline.txt` the installed system needs, optionally its
  boot partition (default 1) and the console pattern that means it is up
  (default `login: `).
- **`site.yaml`:** the node with a `console` and a `disk` (optionally a
  `hostname`, default its name), `sshKeys` (public keys for root on
  installed nodes), and one network with the `vlan` the node's port must be
  on. `install` checks the port's PVID and stops if it is wrong; it does
  not move ports.

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
   installed system's `cmdline.txt` (see First-boot settings); the image
   under `images/`. The Pi firmware picks the
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
9. **First boot.** Stop serving (so the node isn't answered), power off and
   on: the bootloader's network attempt gets no answer and it falls through
   to the disk (about 45 s on k52), and the installed OS boots. `install`
   waits for the image's `ready` pattern on the console.

`install` follows the console throughout (`logs/<node>.log`; serve's log is
`logs/<node>-serve.log`). On a `STAGE0-ERROR` line it powers the node off
and stops. The steps can also be run by hand with `build initrd`, `build
seed`, `serve`, `power` and `console`.

## First-boot settings

The installed system's `cmdline.txt` is the image's `cmdline` plus:

- `systemd.hostname=<hostname>`;
- `systemd.set_credential_binary=ssh.authorized_keys.root:<base64>`: the
  site's `sshKeys`, which systemd's `provision.conf` tmpfiles rule writes
  to `/root/.ssh/authorized_keys` at boot (base64 because keys contain
  spaces). Public keys only: any user can read `/proc/cmdline`.

Nothing is mounted or written inside the installed root filesystem. On k52
(Rocky 10.2) the result logs in as `ssh root@<node>` with a site key and
shows `k52 login:` on the console. The kernel takes at most 2047 bytes of
command line, which bounds the number of keys.

## Reaching the installed node

After the install nothing gives the node an IPv4 address on the
provisioning network (deferred: OO1 in [`ROADMAP.md`](../ROADMAP.md#next));
it is reached by its IPv6 link-local address. The address is a
NetworkManager "stable privacy" one, new with every install, so stage0
finds it on the wire: `install` prints it at the end, and

```
$ ./stage0 get address k52
k52: fe80::85bf:1b4d:1ae2:1427%eth0.3
  ssh root@fe80::85bf:1b4d:1ae2:1427%eth0.3
```

looks it up any time (an echo to `ff02::1` on the network's interface, then
the neighbour table, matched against the MACs the agent reported; no root
needed). With the lab key, add `-i ~/.cache/stage0/ssh/id_ed25519` to the
`ssh` command.

`install` keeps a record of each node it installs (the agent's report,
the result) in `~/.local/state/stage0/nodes/<node>.json`
(`$XDG_STATE_HOME`, or `--state`); `get address` needs it for the MAC.

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

## Limits

- **TFTP time:** the Pi bootloader gives each TFTP file 60 s, about 180 MB
  at the 3 MB/s it gets here, so an image inside `initramfs8` must stay
  under that. Over HTTP the 994 MB Rocky image took 8.5 s.
- **RAM:** the image is held in RAM before it is written (the agent's
  root is the initramfs), so it must fit the node's memory.
- **Seed:** goes to partition 1 only, files and directories only.
- **Leases** live in `serve`'s memory and are lost when it stops.
