package gs316ep

import (
	"context"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/middelkoopt/stage0/internal/netswitch"
)

// Writes post the same forms as the switch's web UI, as sent by the older lab
// switch.py, which has been used on this switch. The switch's replies say
// nothing useful, so every write is checked by reading the switch back in
// the same session.

// request is one form posted to a page.
type request struct {
	page string // "vlan" or "poePortConf"
	form form
}

// form is a form's fields in the order they are sent. The switch ignores a
// form whose fields come in another order (sorted, as url.Values encodes
// them, does not work), so forms keep the order switch.py sends. The one
// exception is setPvid, whose port field is PORT (the web UI's), not
// switch.py's portid, which the switch ignores.
type form []field

type field struct{ name, value string }

func (f form) encode() string {
	var b strings.Builder
	for i, fl := range f {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(url.QueryEscape(fl.name) + "=" + url.QueryEscape(fl.value))
	}
	return b.String()
}

// get returns the value of the first field named name.
func (f form) get(name string) string {
	for _, fl := range f {
		if fl.name == name {
			return fl.value
		}
	}
	return ""
}

// SetPoE turns a port's PoE on or off, keeping its power limit.
func (c *Client) SetPoE(ctx context.Context, port int, on bool) (netswitch.Change, error) {
	return c.change(ctx, port, func(st netswitch.Status, pages map[string][]byte) (netswitch.Port, []request, error) {
		want, err := st.PlanPoE(port, on)
		if err != nil {
			return want, nil, err
		}
		if before, _ := st.PortID(port); *before.PoE == on {
			return want, nil, nil
		}
		limits, err := parsePoELimits(pages["poePortConf"])
		if err != nil {
			return want, nil, fmt.Errorf("PoE page: %w", err)
		}
		limit, ok := limits[port]
		if !ok {
			return want, nil, fmt.Errorf("port %d: no PoE power limit on the PoE page", port)
		}
		return want, []request{{"poePortConf", poeForm(port, on, limit)}}, nil
	})
}

// CyclePoE turns a port's PoE off and on again (the switch's "reset").
func (c *Client) CyclePoE(ctx context.Context, port int) (netswitch.Change, error) {
	return c.change(ctx, port, func(st netswitch.Status, _ map[string][]byte) (netswitch.Port, []request, error) {
		p, ok := st.PortID(port)
		switch {
		case !ok:
			return p, nil, fmt.Errorf("no port %d", port)
		case p.PoE == nil:
			return p, nil, fmt.Errorf("port %d has no PoE", port)
		case !*p.PoE:
			return p, nil, fmt.Errorf("port %d: PoE is off, so there is nothing to cycle; turn it on instead", port)
		}
		form, err := resetForm(st, port)
		return p, []request{{"poePortConf", form}}, err
	})
}

// SetVLANs sets a port's untagged VLAN (PVID) and its tagged VLANs; the port
// leaves every other VLAN.
func (c *Client) SetVLANs(ctx context.Context, port, pvid int, tagged []int) (netswitch.Change, error) {
	return c.change(ctx, port, func(st netswitch.Status, pages map[string][]byte) (netswitch.Port, []request, error) {
		want, err := st.PlanVLANs(port, pvid, tagged)
		if err != nil {
			return want, nil, err
		}
		members, err := parseMembers(pages["vlan"], len(st.Ports))
		if err != nil {
			return want, nil, fmt.Errorf("VLAN page: %w", err)
		}
		return want, vlanForms(st, members, want), nil
	})
}

// planFunc decides, from the switch's current state, how the port should end
// up and which requests get it there.
type planFunc func(st netswitch.Status, pages map[string][]byte) (want netswitch.Port, reqs []request, err error)

// change runs one write in one session: read, plan, post, read back, check.
func (c *Client) change(ctx context.Context, port int, plan planFunc) (ch netswitch.Change, err error) {
	s, err := c.login(ctx)
	if err != nil {
		return ch, err
	}
	defer func() {
		if lerr := s.logout(ctx); lerr != nil && err == nil {
			err = lerr
		}
	}()

	before, pages, err := s.status(ctx)
	if err != nil {
		return ch, err
	}
	want, reqs, err := plan(before, pages)
	if err != nil {
		return ch, fmt.Errorf("gs316ep: %w", err)
	}
	ch.Before, _ = before.PortID(port)
	if len(reqs) == 0 {
		ch.After = ch.Before
		return ch, nil
	}
	for _, r := range reqs {
		if err := s.post(ctx, r.page, r.form); err != nil {
			return ch, err
		}
	}

	after, afterPages, err := s.status(ctx)
	if err != nil {
		return ch, fmt.Errorf("%w (reading back after the write)", err)
	}
	ch.After, _ = after.PortID(port)
	if err := check(before, after, want); err != nil {
		return ch, err
	}

	// The port table can't tell tagged from untagged-but-not-PVID, so the
	// VLAN membership masks are checked too: unchanged, except the written
	// port's digits after a VLAN write.
	beforeM, err := parseMembers(pages["vlan"], len(before.Ports))
	if err != nil {
		return ch, fmt.Errorf("gs316ep: VLAN page: %w", err)
	}
	afterM, err := parseMembers(afterPages["vlan"], len(after.Ports))
	if err != nil {
		return ch, fmt.Errorf("gs316ep: VLAN page (reading back after the write): %w", err)
	}
	expect := beforeM
	if slices.ContainsFunc(reqs, func(r request) bool { return r.page == "vlan" }) {
		expect = plannedMembers(beforeM, want)
	}
	return ch, checkMembers(expect, afterM, port)
}

// plannedMembers is members with port want.ID's digits set to its planned
// membership.
func plannedMembers(members map[int]string, want netswitch.Port) map[int]string {
	out := make(map[int]string, len(members))
	for id, m := range members {
		out[id] = withMember(m, want.ID, membership(want, id))
	}
	return out
}

// checkMembers confirms the VLAN membership masks read back are the expected
// ones; port is the port that was written.
func checkMembers(expect, got map[int]string, port int) error {
	for _, id := range slices.Sorted(maps.Keys(expect)) {
		e, g := expect[id], got[id]
		if len(g) != len(e) {
			return fmt.Errorf("gs316ep: VLAN %d: membership %q after the write, wanted %q", id, g, e)
		}
		for i := range e {
			if e[i] == g[i] {
				continue
			}
			if i+1 == port {
				return fmt.Errorf("gs316ep: port %d in VLAN %d did not change as planned: now %s, wanted %s", port, id, memberName(g[i]), memberName(e[i]))
			}
			return fmt.Errorf("gs316ep: port %d also changed in VLAN %d: now %s, was %s", i+1, id, memberName(g[i]), memberName(e[i]))
		}
	}
	if len(got) != len(expect) {
		return fmt.Errorf("gs316ep: switch has %d VLANs after the write, wanted %d", len(got), len(expect))
	}
	return nil
}

// memberName names a membership mask digit.
func memberName(d byte) string {
	return map[byte]string{'1': "tagged", '2': "untagged", '3': "not a member"}[d]
}

// check confirms the written port matches the plan and no other port's
// configuration changed.
func check(before, after netswitch.Status, want netswitch.Port) error {
	if len(after.Ports) != len(before.Ports) {
		return fmt.Errorf("gs316ep: switch reports %d ports after the write, %d before", len(after.Ports), len(before.Ports))
	}
	for i, b := range before.Ports {
		a := after.Ports[i]
		if b.ID == want.ID {
			if !netswitch.SameConfig(a, want) {
				return fmt.Errorf("gs316ep: port %d did not change as planned: now %s, wanted %s", want.ID, portConfig(a), portConfig(want))
			}
			continue
		}
		if !netswitch.SameConfig(a, b) {
			return fmt.Errorf("gs316ep: port %d also changed: was %s, now %s", b.ID, portConfig(b), portConfig(a))
		}
	}
	return nil
}

// portConfig summarizes a port's configuration for error messages.
func portConfig(p netswitch.Port) string {
	poe := "no PoE"
	if p.PoE != nil {
		poe = map[bool]string{true: "PoE on", false: "PoE off"}[*p.PoE]
	}
	return fmt.Sprintf("%q %s, PVID %d, tagged %v", p.Name, poe, p.PVID, p.Tagged)
}

// vlanForms returns the requests that give a port exactly the planned VLAN
// membership, posting only VLANs whose membership changes. The order is the
// one switch.py uses: join the new PVID's VLAN, move the PVID, then update
// the old PVID's VLAN and the rest.
func vlanForms(st netswitch.Status, members map[int]string, want netswitch.Port) []request {
	var reqs []request
	update := func(id int) {
		old := members[id]
		mask := withMember(old, want.ID, membership(want, id))
		if old == "" || mask == old {
			return
		}
		v, _ := st.VLAN(id)
		reqs = append(reqs, request{"vlan", form{
			{"page", "adv8021QPage"}, {"ACTION", "add"},
			{"VLAN_ID", strconv.Itoa(id)}, {"VLAN_NAME", v.Name}, {"hiddenMem", mask},
			{"voiceVLANID", "0"}, {"autoCameraVLANID", "0"}, {"autoWifiVLANID", "0"},
			{"fsVoiceVlanCos", "6"}, {"fsAutoCameraVlanCos", "6"}, {"fsAutoWifiVlanCos", "6"},
		}})
	}

	before, _ := st.PortID(want.ID)
	update(want.PVID)
	if want.PVID != before.PVID {
		reqs = append(reqs, request{"vlan", form{
			{"page", "adv8021QPage"}, {"ACTION", "setPvid"},
			{"PORT", strconv.Itoa(want.ID)}, {"PVID", strconv.Itoa(want.PVID)},
		}})
		update(before.PVID)
	}
	for _, id := range slices.Sorted(maps.Keys(members)) {
		if id != want.PVID && id != before.PVID {
			update(id)
		}
	}
	return reqs
}

// membership is the mask digit for port p in VLAN id.
func membership(p netswitch.Port, id int) byte {
	switch {
	case p.PVID == id:
		return '2'
	case slices.Contains(p.Tagged, id):
		return '1'
	}
	return '3'
}

// withMember returns mask with port's digit set to d (port 1 is the first digit).
func withMember(mask string, port int, d byte) string {
	b := []byte(mask)
	b[port-1] = d
	return string(b)
}

// poeForm sets one port's PoE admin state; limit is its current power limit
// in tenths of a watt, sent back unchanged.
func poeForm(port int, on bool, limit string) form {
	state := map[bool]string{true: "1", false: "0"}[on]
	return form{
		{"TYPE", "submitPoe"}, {"PORT_NO", strconv.Itoa(port)}, {"POWER_LIMIT_VALUE", limit},
		{"PRIORITY", "NOTSET"}, {"POWER_MODE", "NOTSET"}, {"POWER_LIMIT_TYPE", "NOTSET"},
		{"DETECTION", "NOTSET"}, {"ADMIN_STATE", state}, {"DISCONNECT_TYPE", "NOTSET"},
	}
}

// resetForm power-cycles one port: a mask with one digit per PoE port. PoE
// ports must be numbered from 1 without gaps (ports 1-15 on a GS316EP).
func resetForm(st netswitch.Status, port int) (form, error) {
	var mask strings.Builder
	for _, p := range st.Ports {
		if p.PoE == nil {
			continue
		}
		if p.ID != mask.Len()+1 {
			return nil, fmt.Errorf("PoE ports are not numbered 1 to n (port %d)", p.ID)
		}
		mask.WriteByte(map[bool]byte{true: '1', false: '0'}[p.ID == port])
	}
	return form{{"TYPE", "resetPoe"}, {"PoePort", mask.String()}}, nil
}
