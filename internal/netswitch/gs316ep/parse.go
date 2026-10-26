package gs316ep

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/middelkoopt/stage0/internal/netswitch"
)

// parseStatus builds a Status from the three status pages. It is strict:
// any text it doesn't recognize is an error, so a firmware change shows up
// as a failure rather than as wrong data.
func parseStatus(dashboard, poe, vlan []byte) (netswitch.Status, error) {
	ports, err := parseDashboard(dashboard)
	if err != nil {
		return netswitch.Status{}, fmt.Errorf("dashboard page: %w", err)
	}
	if err := parsePoE(poe, ports); err != nil {
		return netswitch.Status{}, fmt.Errorf("PoE page: %w", err)
	}
	vlans, err := parseVLANs(vlan, ports)
	if err != nil {
		return netswitch.Status{}, fmt.Errorf("VLAN page: %w", err)
	}
	return netswitch.Status{Ports: ports, VLANs: vlans}, nil
}

// parseDashboard reads port number, label, link state and speed.
func parseDashboard(page []byte) ([]netswitch.Port, error) {
	elems, err := scan(page)
	if err != nil {
		return nil, err
	}
	var ports []netswitch.Port
	var cur *netswitch.Port
	for _, e := range elems {
		switch {
		case e.hasClass("port-number"):
			id, err := strconv.Atoi(e.text)
			if err != nil {
				return nil, fmt.Errorf("port number %q: %w", e.text, err)
			}
			if id != len(ports)+1 {
				return nil, fmt.Errorf("port %d out of order", id)
			}
			ports = append(ports, netswitch.Port{ID: id, Tagged: []int{}})
			cur = &ports[len(ports)-1]
		case cur == nil:
			continue
		case e.hasClass("name"):
			cur.Name = e.text
		case e.hasClass("status-on-port"):
			switch e.text {
			case "CONNECTED":
				cur.Linked = true
			case "AVAILABLE":
				cur.Linked = false
			default:
				return nil, fmt.Errorf("port %d: unknown link status %q", cur.ID, e.text)
			}
		case e.hasClass("link-speed-text"):
			if e.text != "No Speed" {
				cur.Speed = e.text
			}
		}
	}
	if len(ports) == 0 {
		return nil, fmt.Errorf("no ports found")
	}
	return ports, nil
}

// parsePoE fills in the PoE admin state. Ports absent from the page have no PoE.
func parsePoE(page []byte, ports []netswitch.Port) error {
	elems, err := scan(page)
	if err != nil {
		return err
	}
	var cur *netswitch.Port
	found := 0
	for _, e := range elems {
		switch {
		case e.hasClass("port-number"):
			// Text is "1 - k52", or just "3" for an unlabeled port.
			f := strings.Fields(e.text)
			if len(f) == 0 {
				return fmt.Errorf("empty port number")
			}
			id, err := strconv.Atoi(f[0])
			if err != nil || id < 1 || id > len(ports) {
				return fmt.Errorf("port number %q", e.text)
			}
			cur = &ports[id-1]
		case cur != nil && e.hasClass("admin-state"):
			var on bool
			switch e.text {
			case "Enable":
				on = true
			case "Disable":
				on = false
			default:
				return fmt.Errorf("port %d: unknown PoE state %q", cur.ID, e.text)
			}
			cur.PoE = &on
			found++
			cur = nil
		}
	}
	if found == 0 {
		return fmt.Errorf("no PoE ports found")
	}
	return nil
}

// parseVLANs reads the VLAN list and each port's membership.
func parseVLANs(page []byte, ports []netswitch.Port) ([]netswitch.VLAN, error) {
	elems, err := scan(page)
	if err != nil {
		return nil, err
	}
	var vlans []netswitch.VLAN
	var cur *netswitch.Port
	for _, e := range elems {
		if _, ok := e.attr("list-vid"); ok {
			id, err := strconv.Atoi(e.text)
			if err != nil {
				return nil, fmt.Errorf("VLAN id %q: %w", e.text, err)
			}
			vlans = append(vlans, netswitch.VLAN{ID: id})
			continue
		}
		if _, ok := e.attr("list-vnm"); ok {
			if len(vlans) == 0 {
				return nil, fmt.Errorf("VLAN name %q before any id", e.text)
			}
			vlans[len(vlans)-1].Name = e.text
			continue
		}
		switch {
		case e.hasClass("port-count"):
			id, err := strconv.Atoi(e.text)
			if err != nil || id < 1 || id > len(ports) {
				return nil, fmt.Errorf("port number %q", e.text)
			}
			cur = &ports[id-1]
		case cur != nil && e.hasClass("pvid-table-vlan-list"):
			pvid, tagged, err := parseVLANList(e.text)
			if err != nil {
				return nil, fmt.Errorf("port %d: %w", cur.ID, err)
			}
			cur.PVID, cur.Tagged = pvid, tagged
			cur = nil
		}
	}
	if len(vlans) == 0 {
		return nil, fmt.Errorf("no VLANs found")
	}
	for _, p := range ports {
		if p.PVID == 0 {
			return nil, fmt.Errorf("port %d: no VLAN membership found", p.ID)
		}
	}
	return vlans, nil
}

// parseVLANList parses a port's membership, e.g. "3, 5*, 23": the starred
// VLAN is the PVID (untagged), the rest are tagged.
func parseVLANList(s string) (pvid int, tagged []int, err error) {
	tagged = []int{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		star := strings.HasSuffix(item, "*")
		id, err := strconv.Atoi(strings.TrimSuffix(item, "*"))
		if err != nil {
			return 0, nil, fmt.Errorf("VLAN list %q: %w", s, err)
		}
		if !star {
			tagged = append(tagged, id)
			continue
		}
		if pvid != 0 {
			return 0, nil, fmt.Errorf("VLAN list %q: more than one PVID", s)
		}
		pvid = id
	}
	if pvid == 0 {
		return 0, nil, fmt.Errorf("VLAN list %q: no PVID", s)
	}
	slices.Sort(tagged)
	return pvid, tagged, nil
}

// parseMembers reads each VLAN's membership mask from the VLAN page: one digit
// per port, port 1 first; 1 = tagged, 2 = untagged, 3 = not a member. This is
// the switch's own record, and the format it accepts back.
func parseMembers(page []byte, nports int) (map[int]string, error) {
	elems, err := scan(page)
	if err != nil {
		return nil, err
	}
	members := make(map[int]string)
	vlan := 0
	for _, e := range elems {
		if _, ok := e.attr("list-vid"); ok {
			if vlan, err = strconv.Atoi(e.text); err != nil {
				return nil, fmt.Errorf("VLAN id %q: %w", e.text, err)
			}
			continue
		}
		if _, ok := e.attr("list-vhidmem"); !ok {
			continue
		}
		mask, _ := e.attr("value")
		if vlan == 0 {
			return nil, fmt.Errorf("membership %q before any VLAN id", mask)
		}
		if len(mask) != nports || strings.Trim(mask, "123") != "" {
			return nil, fmt.Errorf("VLAN %d: membership %q is not %d digits of 1-3", vlan, mask, nports)
		}
		members[vlan] = mask
		vlan = 0
	}
	if len(members) == 0 {
		return nil, fmt.Errorf("no VLAN membership found")
	}
	return members, nil
}

// parsePoELimits reads each PoE port's power limit, in the tenths of a watt
// the switch accepts back ("30.0" W -> "300").
func parsePoELimits(page []byte) (map[int]string, error) {
	elems, err := scan(page)
	if err != nil {
		return nil, err
	}
	limits := make(map[int]string)
	port := 0
	for _, e := range elems {
		switch {
		case e.hasClass("port-number"):
			f := strings.Fields(e.text)
			if len(f) == 0 {
				return nil, fmt.Errorf("empty port number")
			}
			if port, err = strconv.Atoi(f[0]); err != nil {
				return nil, fmt.Errorf("port number %q", e.text)
			}
		case port != 0 && e.hasClass("Power-Limit-text"):
			w, tenth, ok := strings.Cut(e.text, ".")
			n, err := strconv.Atoi(w + tenth)
			if !ok || len(tenth) != 1 || err != nil || n < 0 {
				return nil, fmt.Errorf("port %d: power limit %q", port, e.text)
			}
			limits[port] = strconv.Itoa(n)
			port = 0
		}
	}
	if len(limits) == 0 {
		return nil, fmt.Errorf("no PoE power limits found")
	}
	return limits, nil
}
