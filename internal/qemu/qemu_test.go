package qemu

import (
	"strings"
	"testing"
)

func testVM() VM {
	return VM{
		Memory: 2048, CPUs: 2, Disk: "/c/vm1.raw", MAC: "52:54:00:12:34:56",
		Switch: "/c/vnet.sock", Console: "/c/vm1.console", Firmware: "/fw/AAVMF_CODE.fd", Vars: "/c/vm1.vars",
	}
}

func TestNetArgs(t *testing.T) {
	got := strings.Join(testVM().NetArgs(), " ")
	for _, want := range []string{
		"-M virt -accel kvm -cpu host -m 2048 -smp 2 -display none",
		"-chardev socket,id=console,path=/c/vm1.console,server=on,wait=on -serial chardev:console",
		"-drive if=pflash,format=raw,readonly=on,file=/fw/AAVMF_CODE.fd -drive if=pflash,format=raw,file=/c/vm1.vars",
		"-drive file=/c/vm1.raw,if=none,id=disk,format=raw -device virtio-blk-pci,drive=disk,bootindex=1",
		"-netdev stream,id=net0,server=off,addr.type=unix,addr.path=/c/vnet.sock",
		"-device virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56,bootindex=0",
		"-no-reboot",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-kernel") {
		t.Errorf("netboot loads a kernel itself: %s", got)
	}
}

func TestDiskArgs(t *testing.T) {
	got := strings.Join(testVM().DiskArgs("ds=nocloud;s=http://10.0.9.1/nocloud/vm1/"), " ")
	for _, want := range []string{
		"-drive if=pflash,format=raw,readonly=on,file=/fw/AAVMF_CODE.fd -drive if=pflash,format=raw,file=/c/vm1.vars",
		"-smbios type=1,serial=ds=nocloud;s=http://10.0.9.1/nocloud/vm1/",
		"-drive file=/c/vm1.raw,if=none,id=disk,format=raw -device virtio-blk-pci,drive=disk,bootindex=0",
		"-device virtio-net-pci,netdev=net0,mac=52:54:00:12:34:56 ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("args lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "-kernel") {
		t.Errorf("disk boot loads a kernel: %s", got)
	}
}
