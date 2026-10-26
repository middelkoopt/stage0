# How an install works

How stage0 installs a node today, as built and run on k52 (Raspberry Pi 5,
Rocky 10.2). Each step is a stage0 command; the sequence is run by hand
until `stage0 install <node>` exists (see [`ROADMAP.md`](../ROADMAP.md)).
Platform facts are in [`netboot.md`](netboot.md), image facts in
[`images.md`](images.md), code layout in [`development.md`](development.md).

> Last verified: 2026-10-26

## The sequence

1. **Network.** The node's switch port has PVID `pinet` (`stage0 set port`);
   the host's `eth0.3` carries `10.0.3.1/24` (systemd-networkd, not stage0).
2. **Serve.** `stage0 serve pinet --root DIR` answers DHCPv4, TFTP and HTTP
   on `eth0.3` only, as an ordinary user, logging every request.
3. **Netboot.** Powered on (`stage0 power on`), the Pi bootloader sends a
   PXE DHCP request; stage0 leases it an address with the Pi's option 43,
   and the bootloader fetches `config.txt`, the kernel and `initramfs8` over
   TFTP.
4. **Agent.** `initramfs8` is built by `stage0 build initrd`: the static
   stage0 binary as `/init`. Run as PID 1 it is the agent: it mounts
   `/proc`, `/sys`, `/dev`, waits for disks to settle and prints
   `STAGE0-REPORT {json}` on the console (model, serial, kernel, memory,
   filesystems, disks, interfaces).
5. **Image.** With `stage0.url=` the kernel brings up the network
   (`ip=dhcp`; stage0 answers it because the same MAC just PXE-booted) and
   the agent downloads the image over HTTP into RAM. Without it, the image
   is `/payload` inside `initramfs8` and the network is not used after the
   bootloader.
6. **Write.** The agent checks the image's SHA-256 against
   `stage0.sha256=` and only then streams the xz-decompressed image onto
   `/dev/<stage0.disk>`, printing `STAGE0-PROGRESS` lines.
7. **Seed.** With `stage0.seed=`, the agent re-reads the partition table,
   mounts partition 1 (FAT) and unpacks the seed (a cpio from
   `stage0 build seed`) over it, replacing files of the same name. Then it
   prints `STAGE0-INSTALLED {json}` and waits to be powered off.
8. **First boot.** Stop `serve` (so the node isn't answered), power-cycle:
   the bootloader's network attempt gets no answer and it falls through to
   the disk (about 45 s on k52), and the installed OS boots. `stage0 console
   --until 'login: '` waits for it.

`stage0 console <node> --until 'STAGE0-(INSTALLED|ERROR) .*'` follows steps
4-7; every agent failure is a `STAGE0-ERROR` line.

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

## Example: the k52 run

```bash
B=./stage0; ROOT=~/.cache/stage0/boot-install    # Pi boot files from the image
IMG=Rocky-10-SBC-RaspberryPi-10.2-20260525.0.aarch64.raw.xz
$B build initrd --out $ROOT/initramfs8
$B build seed --from seed-k52 --out $ROOT/seed.cpio  # seed-k52/cmdline.txt
echo "console=serial0,115200 ip=dhcp stage0.disk=mmcblk0 stage0.sha256=<sum>" \
     "stage0.url=http://10.0.3.1/images/$IMG stage0.seed=http://10.0.3.1/seed.cpio" \
     > $ROOT/cmdline.txt
$B serve pinet --root $ROOT &
$B power on k52
$B console k52 --until 'STAGE0-(INSTALLED|ERROR) .*'
kill %1; $B power cycle k52
$B console k52 --until 'login: '
```
