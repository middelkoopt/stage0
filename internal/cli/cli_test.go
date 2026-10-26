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
	"testing"

	"github.com/middelkoopt/stage0/internal/config"
	"github.com/middelkoopt/stage0/internal/netswitch"
)

// fakeSwitch serves a fixed status and records writes instead of making them.
type fakeSwitch struct {
	st     netswitch.Status
	err    error
	writes []string
}

func (f *fakeSwitch) Status(context.Context) (netswitch.Status, error) { return f.st, f.err }

func (f *fakeSwitch) SetPoE(_ context.Context, port int, on bool) (netswitch.Change, error) {
	f.writes = append(f.writes, fmt.Sprintf("poe %d %v", port, on))
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

// testSite names one switch and two nodes, one with a console.
const testSite = `
switches:
  - {name: lab, address: switch.example}
nodes:
  - {name: k53, console: {device: /dev/serial/by-id/probe}}
  - {name: k41}
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
	cmd.SetArgs(append([]string{"--site", site, "--secrets", filepath.Join(dir, ".secrets")}, args...))
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
