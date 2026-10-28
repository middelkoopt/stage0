package netswitch

import (
	"reflect"
	"strings"
	"testing"
)

func testStatus() Status {
	on, off := true, false
	return Status{
		Ports: []Port{
			{ID: 1, Name: "k52", PoE: &off, PVID: 5, Tagged: []int{3, 23}},
			{ID: 2, Name: "k53", Linked: true, Speed: "1000M Full", PoE: &on, PVID: 3, Tagged: []int{5, 23}},
			{ID: 3, PoE: &off, PVID: 5, Tagged: []int{3, 23}},
			{ID: 16, PVID: 1, Tagged: []int{23}},
		},
		VLANs: []VLAN{{ID: 1, Name: "Default"}, {ID: 3, Name: "pinet"}, {ID: 5, Name: "piwulf"}, {ID: 23, Name: "coffee"}},
	}
}

func TestFind(t *testing.T) {
	st := testStatus()
	for ref, want := range map[string]int{"k53": 2, "2": 2, "3": 3, "16": 16} {
		p, err := st.Find(ref)
		if err != nil || p.ID != want {
			t.Errorf("Find(%q) = port %d, %v; want port %d", ref, p.ID, err, want)
		}
	}
	for _, ref := range []string{"k99", "17", "0", ""} {
		if _, err := st.Find(ref); err == nil {
			t.Errorf("Find(%q): want error", ref)
		}
	}
}

func TestFindPrefersLabel(t *testing.T) {
	st := testStatus()
	st.Ports[0].Name = "2" // a label that looks like a port number wins
	if p, _ := st.Find("2"); p.ID != 1 {
		t.Errorf("Find(\"2\") = port %d, want the port labeled 2", p.ID)
	}
}

func TestFindDuplicateLabel(t *testing.T) {
	st := testStatus()
	st.Ports[2].Name = "k53"
	if _, err := st.Find("k53"); err == nil || !strings.Contains(err.Error(), "ports 2 and 3") {
		t.Errorf("err = %v, want an ambiguity error naming both ports", err)
	}
}

func TestFindVLAN(t *testing.T) {
	st := testStatus()
	for ref, want := range map[string]int{"pinet": 3, "3": 3, "coffee": 23, "1": 1} {
		v, err := st.FindVLAN(ref)
		if err != nil || v.ID != want {
			t.Errorf("FindVLAN(%q) = %d, %v; want %d", ref, v.ID, err, want)
		}
	}
	for _, ref := range []string{"cable", "4", ""} {
		if _, err := st.FindVLAN(ref); err == nil {
			t.Errorf("FindVLAN(%q): want error", ref)
		}
	}
}

func TestPlanPoE(t *testing.T) {
	st := testStatus()
	p, err := st.PlanPoE(1, true)
	if err != nil {
		t.Fatal(err)
	}
	if p.PoE == nil || !*p.PoE {
		t.Errorf("planned PoE = %v, want on", p.PoE)
	}
	if *st.Ports[0].PoE {
		t.Error("PlanPoE changed the status it was given")
	}
	if _, err := st.PlanPoE(16, true); err == nil || !strings.Contains(err.Error(), "no PoE") {
		t.Errorf("port without PoE: err = %v", err)
	}
	if _, err := st.PlanPoE(17, true); err == nil {
		t.Error("unknown port: want error")
	}
}

func TestPlanVLANs(t *testing.T) {
	st := testStatus()
	p, err := st.PlanVLANs(2, 5, []int{23, 3, 23})
	if err != nil {
		t.Fatal(err)
	}
	if p.PVID != 5 || !reflect.DeepEqual(p.Tagged, []int{3, 23}) {
		t.Errorf("planned = PVID %d tagged %v, want 5 [3 23] (sorted, deduplicated)", p.PVID, p.Tagged)
	}
	if !reflect.DeepEqual(st.Ports[1].Tagged, []int{5, 23}) {
		t.Error("PlanVLANs changed the status it was given")
	}
	for name, c := range map[string]struct {
		pvid   int
		tagged []int
		want   string
	}{
		"unknown pvid":     {4, nil, "VLAN 4"},
		"unknown tagged":   {3, []int{4}, "VLAN 4"},
		"pvid also tagged": {3, []int{3}, "both"},
	} {
		if _, err := st.PlanVLANs(2, c.pvid, c.tagged); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to mention %q", name, err, c.want)
		}
	}
}

func TestSameConfig(t *testing.T) {
	st := testStatus()
	a := st.Ports[1]
	b := a
	b.Linked, b.Speed = false, "" // link state is not configuration
	if !SameConfig(a, b) {
		t.Error("link state changes should not count")
	}
	off := false
	for name, change := range map[string]func(*Port){
		"poe":    func(p *Port) { p.PoE = &off },
		"pvid":   func(p *Port) { p.PVID = 5 },
		"tagged": func(p *Port) { p.Tagged = []int{23} },
		"name":   func(p *Port) { p.Name = "other" },
	} {
		c := a
		change(&c)
		if SameConfig(a, c) {
			t.Errorf("%s change not detected", name)
		}
	}
}
