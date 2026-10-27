package ipmi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeNode records power changes.
type fakeNode struct {
	delay   time.Duration
	mu      sync.Mutex
	on      bool
	changes []bool
}

func (f *fakeNode) PowerOn(context.Context) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.on, nil
}

func (f *fakeNode) SetPower(_ context.Context, on bool) error {
	time.Sleep(f.delay) // a switch's web UI is slow
	f.mu.Lock()
	defer f.mu.Unlock()
	f.on = on
	f.changes = append(f.changes, on)
	return nil
}

func (f *fakeNode) Console() (Console, error) { return nil, errors.New("no console") }

// waitChanges waits for the node's power changes (done in the background)
// to number n, and returns them.
func (f *fakeNode) waitChanges(n int) []bool {
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		f.mu.Lock()
		c := append([]bool(nil), f.changes...)
		f.mu.Unlock()
		if len(c) >= n {
			return c
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.changes...)
}

// start runs a BMC for node on a random localhost port.
func start(t *testing.T, node Node) int {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{Name: "k52", User: "admin", Password: "s3cret", Node: node, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Serve(ctx, conn); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return conn.LocalAddr().(*net.UDPAddr).Port
}

// ipmitool runs the real ipmitool against the BMC, skipping without it.
func ipmitool(t *testing.T, port int, suite, password string, args ...string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("ipmitool"); err != nil {
		t.Skip("no ipmitool")
	}
	cmd := exec.Command("ipmitool", append([]string{"-I", "lanplus", "-H", "127.0.0.1", "-p", strconv.Itoa(port),
		"-U", "admin", "-P", password, "-C", suite, "-N", "1", "-R", "2"}, args...)...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestIpmitoolPower(t *testing.T) {
	for _, suite := range []string{"3", "17"} {
		node := &fakeNode{}
		port := start(t, node)
		if out, err := ipmitool(t, port, suite, "s3cret", "chassis", "power", "status"); err != nil || !strings.Contains(out, "Chassis Power is off") {
			t.Fatalf("suite %s: status: %v\n%s", suite, err, out)
		}
		if out, err := ipmitool(t, port, suite, "s3cret", "chassis", "power", "on"); err != nil || !strings.Contains(out, "Up/On") {
			t.Errorf("suite %s: on: %v\n%s", suite, err, out)
		}
		node.waitChanges(1)
		if out, err := ipmitool(t, port, suite, "s3cret", "chassis", "power", "status"); err != nil || !strings.Contains(out, "Chassis Power is on") {
			t.Errorf("suite %s: status after on: %v\n%s", suite, err, out)
		}
		if out, err := ipmitool(t, port, suite, "s3cret", "chassis", "power", "cycle"); err != nil || !strings.Contains(out, "Cycle") {
			t.Errorf("suite %s: cycle: %v\n%s", suite, err, out)
		}
		if c := node.waitChanges(3); len(c) != 3 || !c[0] || c[1] || !c[2] {
			t.Errorf("suite %s: power changes %v, want on, off, on", suite, c)
		}
		if out, err := ipmitool(t, port, suite, "s3cret", "mc", "info"); err != nil || !strings.Contains(out, "IPMI Version") {
			t.Errorf("suite %s: mc info: %v\n%s", suite, err, out)
		}
	}
}

func TestIpmitoolWrongPassword(t *testing.T) {
	port := start(t, &fakeNode{})
	if out, err := ipmitool(t, port, "17", "wrong", "chassis", "power", "status"); err == nil {
		t.Errorf("a wrong password was let in:\n%s", out)
	}
}

func TestChecksum(t *testing.T) {
	// A Get Device ID request: rsAddr 0x20, netFn App, rqAddr 0x81.
	b := []byte{0x20, 0x18}
	b = append(b, checksum(b), 0x81, 0x04, 0x01)
	b = append(b, checksum(b[3:]))
	m, ok := parseMessage(b)
	if !ok || m.netFn != netFnApp || m.cmd != cmdGetDeviceID || m.rqSeq != 1 {
		t.Fatalf("parse %+v %v", m, ok)
	}
	r := m.response(ccOK, 0xaa)
	if checksum(r[:2]) != r[2] || checksum(r[3:len(r)-1]) != r[len(r)-1] || r[1] != (netFnApp+1)<<2 {
		t.Errorf("response % x", r)
	}
}

func TestRetransmitRunsOnce(t *testing.T) {
	// ipmitool resends a request it has no answer to within a second; a
	// slow power change must still happen once.
	node := &fakeNode{delay: 1500 * time.Millisecond}
	port := start(t, node)
	if out, err := ipmitool(t, port, "17", "s3cret", "chassis", "power", "cycle"); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	node.waitChanges(2)
	time.Sleep(2 * time.Second) // a second cycle would show by now
	if c := node.waitChanges(2); len(c) != 2 {
		t.Errorf("power changes %v, want one cycle (off, on)", c)
	}
}
