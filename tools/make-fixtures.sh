#!/bin/sh
# Regenerates the test fixtures: small FAT12, FAT16 and FAT32 images holding
# the same files, and disk images with GPT and MBR tables (needs mkfs.fat,
# mtools and sfdisk; tests don't).
set -eu
out=$(cd "$(dirname "$0")/../internal/fat/testdata" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
printf 'arm_64bit=1\n' > "$tmp/config.txt"
# 5000 bytes: more than one cluster, so the cluster chain is followed.
awk 'BEGIN { for (i = 0; i < 500; i++) printf "kernel%04d\n", i }' > "$tmp/kernel_2712.img"
printf 'overlay readme\n' > "$tmp/README"
for spec in "12 1024" "16 8192" "32 40960"; do
	set -- $spec
	img="$tmp/fat$1.img"
	mkfs.fat -F "$1" -s 1 -n STAGE0 -C "$img" "$2" >/dev/null
	mcopy -i "$img" "$tmp/config.txt" "$tmp/kernel_2712.img" ::/
	mmd -i "$img" ::/overlays
	mcopy -i "$img" "$tmp/README" ::/overlays/README
	mcopy -i "$img" "$tmp/config.txt" ::/overlays/a-rather-long-overlay-name.dtbo
	gzip -9n < "$img" > "$out/fat$1.img.gz"
done

# internal/ptable/testdata: 8 MiB disk images with a GPT and an MBR table.
pout=$(cd "$(dirname "$0")/../internal/ptable/testdata" && pwd)
for label in gpt dos; do
	img="$tmp/$label.img"
	truncate -s 8M "$img"
	if [ "$label" = gpt ]; then
		printf 'label: gpt\nstart=2048, size=4096, type=uefi, name="boot"\nstart=6144, size=8192, type=linux, uuid=ae437f98-7402-4a21-9752-30303d4a7a8a, name="root"\n' |
			sfdisk -q "$img"
	else
		printf 'label: dos\nstart=2048, size=4096, type=c, bootable\nstart=6144, size=8192, type=83\n' |
			sfdisk -q "$img"
	fi
	gzip -9n < "$img" > "$pout/$label.img.gz"
done

# internal/qcow2/testdata: a 4 MiB disk with a pattern and a hole, as raw
# (the expected contents) and as qcow2, plain and zlib-compressed.
qout=$(cd "$(dirname "$0")/../internal/qcow2/testdata" && pwd)
raw="$tmp/disk.raw"
truncate -s 4M "$raw"
awk 'BEGIN { for (i = 0; i < 20000; i++) printf "stage0 block %06d\n", i }' | dd of="$raw" conv=notrunc status=none
printf 'tail' | dd of="$raw" bs=1 seek=$((4*1024*1024-4)) conv=notrunc status=none
qemu-img convert -q -f raw -O qcow2 -o cluster_size=65536 "$raw" "$tmp/plain.qcow2"
qemu-img convert -q -c -f raw -O qcow2 -o cluster_size=65536,compression_type=zlib "$raw" "$tmp/zlib.qcow2"
gzip -9n < "$raw" > "$qout/disk.raw.gz"
gzip -9n < "$tmp/plain.qcow2" > "$qout/plain.qcow2.gz"
gzip -9n < "$tmp/zlib.qcow2" > "$qout/zlib.qcow2.gz"
