// Package qemu runs the test bed's VMs: aarch64 QEMU with KVM, a virtio
// disk, a NIC on stage0's virtual switch, and the serial console on a Unix
// socket that QEMU waits for before it starts the VM (so no output is
// missed). A VM boots either stage0's agent directly (-kernel, -initrd;
// the distro firmware can't netboot) or its disk with UEFI.
package qemu

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// VM is one virtual machine's hardware.
type VM struct {
	Memory   int // MB
	CPUs     int
	Disk     string // raw disk image file
	MAC      string
	Switch   string // stage0 vswitch socket
	Console  string // Unix socket QEMU serves the serial console on
	Firmware string // UEFI code (e.g. AAVMF_CODE.fd)
	Vars     string // writable copy of the UEFI variable store
}

// args are the VM's QEMU arguments, booting the network first (the
// agent, by UEFI netboot) or the disk.
func (vm VM) args(netFirst bool) []string {
	disk, nic := ",bootindex=0", ""
	if netFirst {
		disk, nic = ",bootindex=1", ",bootindex=0"
	}
	return []string{
		"-M", "virt", "-accel", "kvm", "-cpu", "host",
		"-m", strconv.Itoa(vm.Memory), "-smp", strconv.Itoa(vm.CPUs), "-display", "none",
		"-chardev", "socket,id=console,path=" + vm.Console + ",server=on,wait=on", "-serial", "chardev:console",
		"-drive", "if=pflash,format=raw,readonly=on,file=" + vm.Firmware,
		"-drive", "if=pflash,format=raw,file=" + vm.Vars,
		"-drive", "file=" + vm.Disk + ",if=none,id=disk,format=raw",
		"-device", "virtio-blk-pci,drive=disk" + disk,
		"-netdev", "stream,id=net0,server=off,addr.type=unix,addr.path=" + vm.Switch,
		"-device", "virtio-net-pci,netdev=net0,mac=" + vm.MAC + nic,
	}
}

// NetArgs boot the agent: the firmware netboots by UEFI HTTP boot
// (stage0's DHCP names the UKI's URL), its PXE turned off (edk2's fw_cfg
// switches), as an unanswered PXE try first costs over a minute. With
// -no-reboot the VM's own reboot ends QEMU.
func (vm VM) NetArgs() []string {
	return append(vm.args(true),
		"-fw_cfg", "name=opt/org.tianocore/IPv4PXESupport,string=n",
		"-fw_cfg", "name=opt/org.tianocore/IPv6PXESupport,string=n",
		"-no-reboot")
}

// DiskArgs boot the disk, with serial (e.g. a cloud-init NoCloud pointer)
// as the SMBIOS system serial number.
func (vm VM) DiskArgs(serial string) []string {
	args := vm.args(false)
	if serial != "" {
		args = append(args, "-smbios", "type=1,serial="+serial)
	}
	return args
}

// Process is a running VM.
type Process struct {
	cmd  *exec.Cmd
	done chan struct{} // closed when QEMU exits
	err  error         // its exit, once done is closed
}

// Start runs qemu-system-aarch64 with args, its own output to log.
func Start(args []string, log io.Writer) (*Process, error) {
	cmd := exec.Command("qemu-system-aarch64", args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("qemu: %w", err)
	}
	p := &Process{cmd: cmd, done: make(chan struct{})}
	go func() {
		p.err = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}

// Done is closed when the VM has exited; Err then says how.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is QEMU's exit error, after Done is closed.
func (p *Process) Err() error { return p.err }

// Stop powers the VM off: SIGTERM, then SIGKILL after 5 seconds.
func (p *Process) Stop() error {
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	select {
	case <-p.done:
		return nil
	case <-time.After(5 * time.Second):
		p.cmd.Process.Kill()
		<-p.done
		return nil
	}
}
