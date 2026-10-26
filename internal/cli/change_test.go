package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestPowerOn(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	out, err := run(t, sw, "power", "on", "k52")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.writes, []string{"poe 1 true"}) {
		t.Errorf("writes = %q", sw.writes)
	}
	if !strings.Contains(out, "k52 (port 1): PoE off -> PoE on") {
		t.Errorf("output:\n%s", out)
	}
}

func TestPowerOffByPortNumber(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	if _, err := run(t, sw, "power", "off", "2"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.writes, []string{"poe 2 false"}) {
		t.Errorf("writes = %q", sw.writes)
	}
}

func TestPowerAlreadyOn(t *testing.T) {
	out, err := run(t, &fakeSwitch{st: testStatus()}, "power", "on", "k53")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "k53 (port 2): already PoE on") {
		t.Errorf("output:\n%s", out)
	}
}

func TestPowerCycle(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	out, err := run(t, sw, "power", "cycle", "k53")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.writes, []string{"cycle 2"}) || !strings.Contains(out, "k53 (port 2): PoE cycled") {
		t.Errorf("writes = %q, output:\n%s", sw.writes, out)
	}
}

func TestPowerCycleOffPortRefusedBeforeWriting(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	_, err := run(t, sw, "power", "cycle", "k52")
	if err == nil || !strings.Contains(err.Error(), "off") || len(sw.writes) != 0 {
		t.Errorf("err = %v, writes = %q; want a PoE-is-off error and no write", err, sw.writes)
	}
}

func TestPowerDryRun(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	out, err := run(t, sw, "power", "on", "k52", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.writes) != 0 {
		t.Errorf("dry run wrote: %q", sw.writes)
	}
	if !strings.Contains(out, "would change k52 (port 1): PoE off -> PoE on") {
		t.Errorf("output:\n%s", out)
	}
}

func TestPowerNoPoE(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	if _, err := run(t, sw, "power", "on", "16"); err == nil || len(sw.writes) != 0 {
		t.Errorf("err = %v, writes = %q; want an error and no write", err, sw.writes)
	}
}

func TestPowerUnknownNode(t *testing.T) {
	if _, err := run(t, &fakeSwitch{st: testStatus()}, "power", "on", "k99"); err == nil {
		t.Error("want error for unknown node")
	}
}

func TestPowerJSON(t *testing.T) {
	out, err := run(t, &fakeSwitch{st: testStatus()}, "power", "on", "k52", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Node    string `json:"node"`
		Port    int    `json:"port"`
		DryRun  bool   `json:"dryRun"`
		Changed bool   `json:"changed"`
		After   struct {
			PoE *bool `json:"poe"`
		} `json:"after"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if r.Node != "k52" || r.Port != 1 || r.DryRun || !r.Changed || r.After.PoE == nil || !*r.After.PoE {
		t.Errorf("result = %+v", r)
	}
}

func TestSetPort(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	out, err := run(t, sw, "set", "port", "k53", "--pvid", "piwulf", "--tagged", "pinet,23")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.writes, []string{"vlans 2 5 [3 23]"}) {
		t.Errorf("writes = %q", sw.writes)
	}
	want := "k53 (port 2): PVID pinet(3), tagged piwulf(5),coffee(23) -> PVID piwulf(5), tagged pinet(3),coffee(23)"
	if !strings.Contains(out, want) {
		t.Errorf("output:\n%s\nwant %s", out, want)
	}
}

func TestSetPortKeepsWhatIsNotGiven(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	if _, err := run(t, sw, "set", "port", "k53", "--tagged", "23"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.writes, []string{"vlans 2 3 [23]"}) {
		t.Errorf("writes = %q; PVID should stay 3", sw.writes)
	}
}

func TestSetPortClearTagged(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	if _, err := run(t, sw, "set", "port", "k53", "--tagged="); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(sw.writes, []string{"vlans 2 3 []"}) {
		t.Errorf("writes = %q; --tagged= should clear tagged VLANs", sw.writes)
	}
}

func TestSetPortNeedsAChange(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	if _, err := run(t, sw, "set", "port", "k53"); err == nil || len(sw.writes) != 0 {
		t.Errorf("err = %v, writes = %q; want an error asking for --pvid or --tagged", err, sw.writes)
	}
}

func TestSetPortUnknownVLAN(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	if _, err := run(t, sw, "set", "port", "k53", "--pvid", "cable"); err == nil || len(sw.writes) != 0 {
		t.Errorf("err = %v, writes = %q; want an error and no write", err, sw.writes)
	}
}

func TestSetPortDryRun(t *testing.T) {
	sw := &fakeSwitch{st: testStatus()}
	out, err := run(t, sw, "set", "port", "k53", "--pvid", "piwulf", "--tagged", "pinet,coffee", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if len(sw.writes) != 0 || !strings.Contains(out, "would change k53") {
		t.Errorf("writes = %q, output:\n%s", sw.writes, out)
	}
}
