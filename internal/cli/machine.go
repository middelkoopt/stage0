package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/image"
	"github.com/middelkoopt/stage0/internal/netswitch"
	"github.com/middelkoopt/stage0/internal/qemu"
	"github.com/middelkoopt/stage0/internal/vswitch"
)

// A machine is how install powers a node and reaches its console: a Pi on
// a PoE switch port, or a QEMU VM on the test bed.
type machine interface {
	name() string // for messages, e.g. "k52 (port 1)"
	port() int    // its switch port, 0 for a VM
	// bootAgent powers it on to netboot (or, for a VM, boot) the agent;
	// bootDisk powers it off and on to boot its disk. follow is handed
	// each console connection made while booting (a VM's console is a new
	// socket every start).
	bootAgent(ctx context.Context, follow func(io.ReadWriteCloser)) error
	bootDisk(ctx context.Context, follow func(io.ReadWriteCloser)) error
	off(ctx context.Context) error
}

// poeMachine is a node on a PoE switch port.
type poeMachine struct {
	sw netswitch.Controller
	p  netswitch.Port
}

func (m *poeMachine) name() string { return label(m.p) }
func (m *poeMachine) port() int    { return m.p.ID }
func (m *poeMachine) bootAgent(ctx context.Context, _ func(io.ReadWriteCloser)) error {
	return powerCycle(ctx, m.sw, m.p.ID)
}
func (m *poeMachine) bootDisk(ctx context.Context, _ func(io.ReadWriteCloser)) error {
	return powerCycle(ctx, m.sw, m.p.ID)
}
func (m *poeMachine) off(ctx context.Context) error {
	_, err := m.sw.SetPoE(ctx, m.p.ID, false)
	return err
}

// powerCycle powers a port off (if it isn't already) and on.
func powerCycle(ctx context.Context, sw netswitch.Controller, port int) error {
	if _, err := sw.SetPoE(ctx, port, false); err != nil {
		return err
	}
	_, err := sw.SetPoE(ctx, port, true)
	return err
}

// newPoEMachine finds the node's switch port, checks it has PoE and is on
// the network's VLAN, moving it there with setPort.
func (a *App) newPoEMachine(ctx context.Context, node config.Node, n config.Network, setPort bool, say func(string, ...any)) (*poeMachine, error) {
	sw, err := a.openSite()
	if err != nil {
		return nil, err
	}
	st, err := sw.Status(ctx)
	if err != nil {
		return nil, err
	}
	port, err := st.Find(node.Name)
	if err != nil {
		return nil, err
	}
	if port.PoE == nil {
		return nil, fmt.Errorf("%s has no PoE, so stage0 can't power it", label(port))
	}
	if n.VLAN != "" {
		v, err := st.FindVLAN(n.VLAN)
		if err != nil {
			return nil, fmt.Errorf("network %q: %w", n.Name, err)
		}
		if port.PVID != v.ID && !setPort {
			return nil, fmt.Errorf("%s has PVID %d, not %s(%d); move it with --set-port or: stage0 set port %s --pvid %s",
				label(port), port.PVID, v.Name, v.ID, node.Name, v.Name)
		}
		if port.PVID != v.ID {
			// The old PVID stays, tagged, so nothing the port carried is lost.
			tagged := slices.DeleteFunc(append(slices.Clone(port.Tagged), port.PVID), func(id int) bool { return id == v.ID })
			slices.Sort(tagged)
			ch, err := sw.SetVLANs(ctx, port.ID, v.ID, tagged)
			if err != nil {
				return nil, err
			}
			say("%s: %s -> %s", label(port), showVLANs(st, ch.Before), showVLANs(st, ch.After))
		}
	}
	return &poeMachine{sw: sw, p: port}, nil
}

// VMProcess is a running VM, as StartVM returns it.
type VMProcess interface{ Stop() error }

// vmMachine is a QEMU VM on the test bed.
type vmMachine struct {
	a      *App
	node   string
	vm     qemu.VM
	serial string // SMBIOS serial for the disk boot (NoCloud pointer)
	log    io.Writer
	proc   VMProcess
}

func (m *vmMachine) name() string { return m.node + " (VM)" }
func (m *vmMachine) port() int    { return 0 }

func (m *vmMachine) bootAgent(ctx context.Context, follow func(io.ReadWriteCloser)) error {
	return m.start(ctx, m.vm.NetArgs(), follow)
}

func (m *vmMachine) bootDisk(ctx context.Context, follow func(io.ReadWriteCloser)) error {
	if err := m.off(ctx); err != nil {
		return err
	}
	return m.start(ctx, m.vm.DiskArgs(m.serial), follow)
}

// start runs QEMU and connects to its console, which QEMU waits for
// before it starts the VM.
func (m *vmMachine) start(ctx context.Context, args []string, follow func(io.ReadWriteCloser)) error {
	os.Remove(m.vm.Console)
	p, err := m.a.StartVM(args, m.log)
	if err != nil {
		return err
	}
	m.proc = p
	for deadline := time.Now().Add(10 * time.Second); ; {
		c, err := m.a.OpenConsole("unix:"+m.vm.Console, 115200)
		if err == nil {
			follow(c)
			return nil
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			m.off(ctx)
			return fmt.Errorf("%s: no console from QEMU: %w", m.node, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (m *vmMachine) off(context.Context) error {
	if m.proc == nil {
		return nil
	}
	err := m.proc.Stop()
	m.proc = nil
	return err
}

// The VMs' UEFI firmware, unless a node names its own: Ubuntu's, as
// Debian's has no network stack and can't netboot (docs/netboot.md).
var uefiFirmware = image.File{
	URL:    "https://archive.ubuntu.com/ubuntu/pool/main/e/edk2/qemu-efi-aarch64_2025.11-3ubuntu7.3_all.deb",
	SHA256: "54c19251290402c23418ec0ed644dfa228c6b5653dbef22ab3e394eeaf740b2b",
}

const (
	uefiFirmwareCode = "/usr/share/AAVMF/AAVMF_CODE.no-secboot.fd"
	uefiFirmwareVars = "/usr/share/AAVMF/AAVMF_VARS.fd" // next to the code, where newVMMachine looks
)

// The EFI stub the agent's UKI is built on: systemd's, from Debian (whose
// pool keeps only the current version: a new point release means a new
// pin).
var ukiStub = image.File{
	URL:    "https://deb.debian.org/debian/pool/main/s/systemd/systemd-boot-efi_257.13-1%7edeb13u1_arm64.deb",
	SHA256: "0e4529d9a72b35c31f34151ef8a0df20f322cfef679f4f5439394e60505b91a2",
}

const ukiStubPath = "/usr/lib/systemd/boot/efi/linuxaa64.efi.stub"

// newVMMachine prepares a VM's files: a fresh, empty disk of the node's
// size, a copy of the firmware's variable store (AAVMF_VARS.fd next to
// firmware), the console socket's place.
func newVMMachine(a *App, node config.Node, firmware, dir, switchSock string, log io.Writer) (*vmMachine, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	v := node.VM
	disk := filepath.Join(dir, node.Name+".raw")
	// Empty (all zeros, sparse): the agent writes only an image's data.
	if err := os.WriteFile(disk, nil, 0o644); err != nil {
		return nil, err
	}
	if err := os.Truncate(disk, int64(v.DiskGB)<<30); err != nil {
		return nil, err
	}
	vars := filepath.Join(dir, node.Name+".vars")
	b, err := os.ReadFile(filepath.Join(filepath.Dir(firmware), "AAVMF_VARS.fd"))
	if err != nil {
		return nil, fmt.Errorf("%s: the firmware's variable store: %w", node.Name, err)
	}
	if err := os.WriteFile(vars, b, 0o644); err != nil {
		return nil, err
	}
	return &vmMachine{a: a, node: node.Name, log: log, vm: qemu.VM{
		Memory: v.Memory, CPUs: v.CPUs, Disk: disk, MAC: v.MAC, Switch: switchSock,
		Console: filepath.Join(dir, node.Name+".console"), Firmware: firmware, Vars: vars,
	}}, nil
}

// runSwitch runs the test bed's switch on sock, with the network's tap
// as a port, until ctx is done.
func (a *App) runSwitch(ctx context.Context, sock, tap string, log *slog.Logger) error {
	os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	p, err := a.OpenTap(tap)
	if err != nil {
		ln.Close()
		return fmt.Errorf("%w (is a `stage0 vswitch` already using it?)", err)
	}
	s := vswitch.New(log)
	go s.Attach(p)
	go func() {
		s.Serve(ctx, ln)
		os.Remove(sock)
	}()
	return nil
}

// startQEMU is the real StartVM.
func startQEMU(args []string, log io.Writer) (VMProcess, error) { return qemu.Start(args, log) }
