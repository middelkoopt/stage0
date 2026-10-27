package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/netswitch"
)

// fakeSwitch serves a fixed status and records writes instead of making them.
type fakeSwitch struct {
	st     netswitch.Status
	err    error
	mu     sync.Mutex
	writes []string
}

// ons counts the PoE-on writes so far.
func (f *fakeSwitch) ons() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, w := range f.writes {
		if strings.HasSuffix(w, " true") {
			n++
		}
	}
	return n
}

func (f *fakeSwitch) Status(context.Context) (netswitch.Status, error) { return f.st, f.err }

func (f *fakeSwitch) SetPoE(_ context.Context, port int, on bool) (netswitch.Change, error) {
	f.mu.Lock()
	f.writes = append(f.writes, fmt.Sprintf("poe %d %v", port, on))
	f.mu.Unlock()
	want, err := f.st.PlanPoE(port, on)
	before, _ := f.st.PortID(port)
	return netswitch.Change{Before: before, After: want}, err
}

func (f *fakeSwitch) CyclePoE(_ context.Context, port int) (netswitch.Change, error) {
	f.writes = append(f.writes, fmt.Sprintf("cycle %d", port))
	p, _ := f.st.PortID(port)
	return netswitch.Change{Before: p, After: p}, nil
}

func (f *fakeSwitch) SetVLANs(_ context.Context, port, pvid int, tagged []int) (netswitch.Change, error) {
	f.writes = append(f.writes, fmt.Sprintf("vlans %d %d %v", port, pvid, tagged))
	want, err := f.st.PlanVLANs(port, pvid, tagged)
	before, _ := f.st.PortID(port)
	return netswitch.Change{Before: before, After: want}, err
}

func testStatus() netswitch.Status {
	on, off := true, false
	return netswitch.Status{
		Ports: []netswitch.Port{
			{ID: 1, Name: "k52", PoE: &off, PVID: 5, Tagged: []int{3, 23}},
			{ID: 2, Name: "k53", Linked: true, Speed: "1000M Full", PoE: &on, PVID: 3, Tagged: []int{5, 23}},
			{ID: 16, PVID: 1, Tagged: []int{}},
		},
		VLANs: []netswitch.VLAN{{ID: 1, Name: "Default"}, {ID: 3, Name: "pinet"}, {ID: 5, Name: "piwulf"}, {ID: 23, Name: "coffee"}},
	}
}

// testSite names one switch, two nodes (one with a console) and a network.
const testSite = `
sshKeys:
  - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExample stage0@test
switches:
  - {name: lab, address: switch.example}
nodes:
  - {name: k53, console: {device: /dev/serial/by-id/probe}, disk: mmcblk0, network: pinet}
  - {name: k41}
  - {name: k52, disk: mmcblk0, network: pinet}
  - {name: vm1, disk: vda, network: vnet, vm: {mac: "52:54:00:12:34:56"}}
networks:
  - {name: pinet, interface: eth0.3, address: 10.0.3.1/24, pool: 10.0.3.100-10.0.3.199, vlan: pinet, prefix6: "fd00:10:3::/64"}
  - {name: vnet, interface: stage0tap0, address: 10.0.9.1/24, pool: 10.0.9.100-10.0.9.199}
`

// run executes the CLI against testSite, with the switch served by sw.
func run(t *testing.T, sw netswitch.Controller, args ...string) (string, error) {
	t.Helper()
	return runApp(t, &App{
		OpenSwitch: func(c config.Switch, _ config.SwitchSecret) (netswitch.Controller, error) {
			if c.Name != "lab" {
				t.Errorf("opened switch %q", c.Name)
			}
			return sw, nil
		},
	}, args...)
}

// runApp executes the CLI against testSite with app's hooks.
func runApp(t *testing.T, app *App, args ...string) (string, error) {
	t.Helper()
	dir := t.TempDir()
	site := filepath.Join(dir, "site.yaml")
	if err := os.WriteFile(site, []byte(testSite), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	app.Stdout = &out
	cmd := app.Command()
	// --state defaults to an empty directory, so tests never read the real
	// install records; a later --state in args wins.
	cmd.SetArgs(append([]string{"--site", site, "--secrets", filepath.Join(dir, ".secrets"),
		"--state", filepath.Join(dir, "state")}, args...))
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestGetPortsText(t *testing.T) {
	out, err := run(t, &fakeSwitch{st: testStatus()}, "get", "ports")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"PORT", "NAME", "LINK", "POE", "PVID", "TAGGED",
		"k53", "up", "1000M Full", "on", "pinet(3)", "piwulf(5),coffee(23)",
		"k52", "down", "off",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// Port 16 has no PoE: shown as "-", not "off".
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && f[0] == "16" && !strings.Contains(line, " - ") {
			t.Errorf("port 16 line %q should show no PoE as -", line)
		}
	}
}

func TestGetPortsJSON(t *testing.T) {
	out, err := run(t, &fakeSwitch{st: testStatus()}, "get", "ports", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var ports []netswitch.Port
	if err := json.Unmarshal([]byte(out), &ports); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if len(ports) != 3 || ports[1].Name != "k53" || ports[2].PoE != nil {
		t.Errorf("ports = %+v", ports)
	}
}

func TestGetVLANs(t *testing.T) {
	out, err := run(t, &fakeSwitch{st: testStatus()}, "get", "vlans")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "pinet") || !strings.Contains(out, "23") {
		t.Errorf("output:\n%s", out)
	}
}

func TestGetPortsSwitchError(t *testing.T) {
	_, err := run(t, &fakeSwitch{err: errors.New("boom")}, "get", "ports")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
}

func TestUnknownOutputFormat(t *testing.T) {
	if _, err := run(t, &fakeSwitch{st: testStatus()}, "get", "ports", "-o", "xml"); err == nil {
		t.Error("want error for -o xml")
	}
}

func TestBMCNode(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	b := &bmcNode{node: config.Node{Name: "k53"}, sw: &poeSwitch{sw: sw}}
	if on, err := b.PowerOn(context.Background()); err != nil || !on {
		t.Errorf("k53 power %v, %v; want on", on, err)
	}
	if err := b.SetPower(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if strings.Join(sw.writes, ",") != "poe 2 false" {
		t.Errorf("writes %v", sw.writes)
	}
	if _, err := (&bmcNode{node: config.Node{Name: "k41"}, sw: &poeSwitch{sw: sw}}).PowerOn(context.Background()); err == nil {
		t.Error("a node with no switch port: want error")
	}
	if _, err := b.Console(); err == nil {
		t.Error("a node with no console: want error")
	}
}

func TestBMCRefuses(t *testing.T) {
	// testSite has no bmc: nodes, and no .secrets.
	if _, err := run(t, &fakeSwitch{st: testStatus()}, "bmc"); err == nil || !strings.Contains(err.Error(), "bmc:") {
		t.Errorf("err = %v", err)
	}
}

func TestGetNodes(t *testing.T) {
	state := t.TempDir()
	os.MkdirAll(filepath.Join(state, "nodes"), 0o755)
	os.WriteFile(filepath.Join(state, "nodes", "k53.json"), []byte(`{"node":"k53","image":"rocky","network":"pinet","mac":"d8:3a:dd:c5:c1:f3","ipv4":"10.0.3.100","address":"fe80::1%eth0.3","port":2}`), 0o644)
	out, err := run(t, &fakeSwitch{st: testStatus()}, "-o", "json", "--state", state, "get", "nodes")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	var nodes []struct {
		Name, Hostname, Network string
		Installed               *struct{ Image, MAC, IPv4, IPv6, Time string }
	}
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if len(nodes) != 4 || nodes[0].Name != "k53" || nodes[0].Installed == nil || nodes[0].Installed.IPv4 != "10.0.3.100" ||
		nodes[0].Installed.MAC != "d8:3a:dd:c5:c1:f3" || nodes[0].Installed.Time == "" || nodes[1].Installed != nil {
		t.Errorf("nodes %+v", nodes)
	}
	out, err = run(t, &fakeSwitch{st: testStatus()}, "--state", state, "get", "nodes")
	if err != nil || !strings.Contains(out, "k53") || !strings.Contains(out, "10.0.3.100") {
		t.Errorf("text: %v\n%s", err, out)
	}
}
