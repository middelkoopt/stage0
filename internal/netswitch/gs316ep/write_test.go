package gs316ep

import (
	"context"
	"maps"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/middelkoopt/stage0/internal/netswitch"
)

func fixtureStatus(t *testing.T) netswitch.Status {
	t.Helper()
	st, err := parseStatus(fixture(t, "dashboard.html"), fixture(t, "poePortConf.html"), fixture(t, "vlan.html"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestParseMembers(t *testing.T) {
	m, err := parseMembers(fixture(t, "vlan.html"), 16)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]string{
		1: "3333333333333332", 2: "3333333322223333", 3: "1212111111113333",
		4: "3333333333333233", 5: "2121222233333333", 23: "1111333311112321",
	}
	if !reflect.DeepEqual(m, want) {
		t.Errorf("members = %v, want %v", m, want)
	}
}

func TestParseMembersRejectsBadMask(t *testing.T) {
	page := []byte(`<span list-vid='4'>7</span><input list-vhidmem='4' value='33X3'>`)
	if _, err := parseMembers(page, 4); err == nil {
		t.Error("bad mask character: want error")
	}
	page = []byte(`<span list-vid='4'>7</span><input list-vhidmem='4' value='333'>`)
	if _, err := parseMembers(page, 4); err == nil {
		t.Error("short mask: want error")
	}
}

func TestParsePoELimits(t *testing.T) {
	l, err := parsePoELimits(fixture(t, "poePortConf.html"))
	if err != nil {
		t.Fatal(err)
	}
	if len(l) != 15 || l[1] != "300" || l[2] != "300" || l[15] != "300" {
		t.Errorf("limits = %v, want 15 ports at 300 (30.0 W)", l)
	}
}

func TestVLANFormsAddTagged(t *testing.T) {
	// Today's real change: tag VLAN 4 (cable) on port 12 (pilab).
	st := fixtureStatus(t)
	members, _ := parseMembers(fixture(t, "vlan.html"), 16)
	want, err := st.PlanVLANs(12, 2, []int{3, 4, 23})
	if err != nil {
		t.Fatal(err)
	}
	got := vlanForms(st, members, want)
	if len(got) != 1 {
		t.Fatalf("got %d requests, want 1 (only VLAN 4 changes): %v", len(got), got)
	}
	wantForm := "page=adv8021QPage&ACTION=add&VLAN_ID=4&VLAN_NAME=cable&hiddenMem=3333333333313233" +
		"&voiceVLANID=0&autoCameraVLANID=0&autoWifiVLANID=0" +
		"&fsVoiceVlanCos=6&fsAutoCameraVlanCos=6&fsAutoWifiVlanCos=6"
	if got[0].page != "vlan" || got[0].form.encode() != wantForm {
		t.Errorf("request = %s %s\nwant vlan %s", got[0].page, got[0].form.encode(), wantForm)
	}
}

func TestVLANFormsMovePVID(t *testing.T) {
	// k53 (port 2) moves from PVID pinet(3) to piwulf(5), keeping pinet and
	// coffee tagged: add to the new PVID's VLAN, set the PVID, then retag the old.
	st := fixtureStatus(t)
	members, _ := parseMembers(fixture(t, "vlan.html"), 16)
	want, err := st.PlanVLANs(2, 5, []int{3, 23})
	if err != nil {
		t.Fatal(err)
	}
	got := vlanForms(st, members, want)
	var steps []string
	for _, r := range got {
		f := r.form
		switch f.get("ACTION") {
		case "add":
			steps = append(steps, "vlan "+f.get("VLAN_ID")+" "+f.get("hiddenMem"))
		case "setPvid":
			steps = append(steps, "pvid "+f.get("PORT")+" "+f.get("PVID"))
		}
	}
	wantSteps := []string{"vlan 5 2221222233333333", "pvid 2 5", "vlan 3 1112111111113333"}
	if !reflect.DeepEqual(steps, wantSteps) {
		t.Errorf("steps = %q, want %q", steps, wantSteps)
	}
	// The switch ignores setPvid with switch.py's "portid" field (checked live).
	if len(got) == 3 && got[1].form.encode() != "page=adv8021QPage&ACTION=setPvid&PORT=2&PVID=5" {
		t.Errorf("setPvid form = %s", got[1].form.encode())
	}
}

func TestCheckMembers(t *testing.T) {
	// k53 (port 2) moving from PVID 3 to 5 with 3 and 23 tagged.
	st := fixtureStatus(t)
	before, _ := parseMembers(fixture(t, "vlan.html"), 16)
	want, _ := st.PlanVLANs(2, 5, []int{3, 23})
	after := plannedMembers(before, want)
	if after[5] != "2221222233333333" || after[3] != "1112111111113333" {
		t.Errorf("planned = %v", after)
	}
	if err := checkMembers(after, after, 2); err != nil {
		t.Errorf("planned result: %v", err)
	}

	// Untagged in both 3 and 5: the port table can't tell this from the plan.
	stuck := maps.Clone(after)
	stuck[3] = withMember(stuck[3], 2, '2')
	if err := checkMembers(after, stuck, 2); err == nil || !strings.Contains(err.Error(), "VLAN 3") {
		t.Errorf("untagged leftover: err = %v, want an error naming VLAN 3", err)
	}

	other := maps.Clone(after)
	other[23] = withMember(other[23], 1, '3')
	if err := checkMembers(after, other, 2); err == nil || !strings.Contains(err.Error(), "port 1") {
		t.Errorf("other port changed: err = %v, want an error naming port 1", err)
	}
}

func TestVLANFormsNoChange(t *testing.T) {
	st := fixtureStatus(t)
	members, _ := parseMembers(fixture(t, "vlan.html"), 16)
	p, _ := st.Find("k53")
	if got := vlanForms(st, members, p); len(got) != 0 {
		t.Errorf("unchanged port sent %d requests", len(got))
	}
}

func TestPoEForm(t *testing.T) {
	// The switch ignores this form when its fields are sorted (checked live).
	got := poeForm(1, true, "300").encode()
	want := "TYPE=submitPoe&PORT_NO=1&POWER_LIMIT_VALUE=300&PRIORITY=NOTSET&POWER_MODE=NOTSET" +
		"&POWER_LIMIT_TYPE=NOTSET&DETECTION=NOTSET&ADMIN_STATE=1&DISCONNECT_TYPE=NOTSET"
	if got != want {
		t.Errorf("form = %s\nwant %s", got, want)
	}
	if poeForm(1, false, "300").get("ADMIN_STATE") != "0" {
		t.Error("off should send ADMIN_STATE 0")
	}
}

func TestResetForm(t *testing.T) {
	st := fixtureStatus(t)
	got, err := resetForm(st, 2)
	if err != nil {
		t.Fatal(err)
	}
	want := "TYPE=resetPoe&PoePort=010000000000000" // one digit per PoE port (1-15)
	if got.encode() != want {
		t.Errorf("form = %s, want %s", got.encode(), want)
	}
}

// Writes against the fake switch. Its onPost hook edits the served pages the
// way the real switch would, so the driver's read-back check sees the result.

func newWriteClient(t *testing.T) (*fakeSwitch, *Client) {
	t.Helper()
	f, srv := newFake(t)
	c, err := New(srv.URL, Credentials{Password: "secret"}, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	return f, c
}

func TestSetPoE(t *testing.T) {
	f, c := newWriteClient(t)
	f.onPost = func(page string, form url.Values) {
		if form.Get("PORT_NO") == "1" && form.Get("ADMIN_STATE") == "1" {
			f.edit("poePortConf.html", "1&nbsp;-&nbsp;k52", `admin-state">Disable<`, `admin-state">Enable<`)
		}
	}
	ch, err := c.SetPoE(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if !ch.Changed() || *ch.Before.PoE || !*ch.After.PoE {
		t.Errorf("change = %+v, want PoE off -> on", ch)
	}
	if len(f.posts) != 1 || f.posts[0].page != "poePortConf.html" ||
		!strings.HasPrefix(f.posts[0].body, "TYPE=submitPoe&") || !strings.HasSuffix(f.posts[0].body, "&Gambit=TESTGAMBIT") {
		t.Errorf("posts = %+v, want one PoE post in form order, with the session token last", f.posts)
	}
	if f.logouts != 1 {
		t.Errorf("logouts = %d, want 1", f.logouts)
	}
}

func TestSetPoEAlreadySet(t *testing.T) {
	f, c := newWriteClient(t)
	ch, err := c.SetPoE(context.Background(), 2, true) // k53 is already on
	if err != nil {
		t.Fatal(err)
	}
	if ch.Changed() || len(f.posts) != 0 {
		t.Errorf("changed=%v posts=%d; an unchanged port should send nothing", ch.Changed(), len(f.posts))
	}
}

func TestSetPoENotApplied(t *testing.T) {
	_, c := newWriteClient(t) // the fake ignores the write
	_, err := c.SetPoE(context.Background(), 1, true)
	if err == nil || !strings.Contains(err.Error(), "port 1") {
		t.Errorf("err = %v, want a read-back error for port 1", err)
	}
}

func TestSetPoEOtherPortChanged(t *testing.T) {
	f, c := newWriteClient(t)
	f.onPost = func(string, url.Values) {
		f.edit("poePortConf.html", "1&nbsp;-&nbsp;k52", `admin-state">Disable<`, `admin-state">Enable<`)
		f.edit("poePortConf.html", `port-number">3<`, `admin-state">Disable<`, `admin-state">Enable<`)
	}
	_, err := c.SetPoE(context.Background(), 1, true)
	if err == nil || !strings.Contains(err.Error(), "port 3") {
		t.Errorf("err = %v, want an error naming port 3", err)
	}
}

func TestSetPoESessionLost(t *testing.T) {
	f, c := newWriteClient(t)
	f.dropOnPost = true
	_, err := c.SetPoE(context.Background(), 1, true)
	if err == nil || !strings.Contains(err.Error(), "session") {
		t.Errorf("err = %v, want a session error", err)
	}
	if f.logouts != 1 {
		t.Errorf("logouts = %d, want 1", f.logouts)
	}
}

func TestCyclePoE(t *testing.T) {
	f, c := newWriteClient(t)
	ch, err := c.CyclePoE(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if ch.Changed() {
		t.Errorf("cycle should leave the configuration as it was: %+v", ch)
	}
	if len(f.posts) != 1 || f.posts[0].form.Get("TYPE") != "resetPoe" {
		t.Errorf("posts = %+v, want one resetPoe", f.posts)
	}
}

func TestCyclePoEOff(t *testing.T) {
	f, c := newWriteClient(t)
	_, err := c.CyclePoE(context.Background(), 1) // k52's PoE is off
	if err == nil || !strings.Contains(err.Error(), "off") {
		t.Errorf("err = %v, want a PoE-is-off error", err)
	}
	if len(f.posts) != 0 {
		t.Errorf("sent %d posts", len(f.posts))
	}
}

func TestSetVLANs(t *testing.T) {
	f, c := newWriteClient(t)
	f.onPost = func(page string, form url.Values) {
		if form.Get("VLAN_ID") == "4" {
			f.edit("vlan.html", "port-count'>12<", "2*, 3, 23<", "2*, 3, 4, 23<")
			f.edit("vlan.html", "list-vid='4'>4<", "3333333333333233", form.Get("hiddenMem"))
		}
	}
	ch, err := c.SetVLANs(context.Background(), 12, 2, []int{3, 4, 23})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ch.After.Tagged, []int{3, 4, 23}) {
		t.Errorf("after = %+v", ch.After)
	}
	if len(f.posts) != 1 || f.posts[0].page != "vlan.html" {
		t.Errorf("posts = %+v, want one VLAN post", f.posts)
	}
}
