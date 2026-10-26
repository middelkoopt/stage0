// Package netswitch is stage0's model of a managed network switch: its ports,
// their PoE state and their VLAN membership. Drivers for specific switches
// live in subpackages and return this model.
package netswitch

import (
	"context"
	"fmt"
	"slices"
	"strconv"
)

// Port is one physical switch port.
type Port struct {
	ID     int    `json:"id"`
	Name   string `json:"name"`            // label set on the switch; matches the machine name
	Linked bool   `json:"linked"`          // link is up
	Speed  string `json:"speed,omitempty"` // negotiated speed, e.g. "1000M Full"; empty when down
	PoE    *bool  `json:"poe,omitempty"`   // PoE admin state; nil when the port has no PoE
	PVID   int    `json:"pvid"`            // untagged (native) VLAN
	Tagged []int  `json:"tagged"`          // tagged VLANs, ascending
}

// VLAN is one 802.1Q VLAN defined on the switch.
type VLAN struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// Status is a snapshot of a switch.
type Status struct {
	Ports []Port `json:"ports"`
	VLANs []VLAN `json:"vlans"`
}

// Port returns the port with the given label.
func (s Status) Port(name string) (Port, bool) {
	for _, p := range s.Ports {
		if p.Name == name {
			return p, true
		}
	}
	return Port{}, false
}

// VLAN returns the VLAN with the given ID.
func (s Status) VLAN(id int) (VLAN, bool) {
	for _, v := range s.VLANs {
		if v.ID == id {
			return v, true
		}
	}
	return VLAN{}, false
}

// Find returns the port a reference names: a port label, or else a port
// number. Labels are machine names, so they are what people and agents use.
func (s Status) Find(ref string) (Port, error) {
	var found []Port
	for _, p := range s.Ports {
		if ref != "" && p.Name == ref {
			found = append(found, p)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		if id, err := strconv.Atoi(ref); err == nil {
			if p, ok := s.PortID(id); ok {
				return p, nil
			}
		}
		return Port{}, fmt.Errorf("no port labeled or numbered %q", ref)
	}
	return Port{}, fmt.Errorf("label %q is on ports %d and %d; use the port number", ref, found[0].ID, found[1].ID)
}

// FindVLAN returns the VLAN a reference names: its name, or else its ID.
func (s Status) FindVLAN(ref string) (VLAN, error) {
	for _, v := range s.VLANs {
		if ref != "" && v.Name == ref {
			return v, nil
		}
	}
	if id, err := strconv.Atoi(ref); err == nil {
		if v, ok := s.VLAN(id); ok {
			return v, nil
		}
	}
	return VLAN{}, fmt.Errorf("no VLAN named or numbered %q", ref)
}

// PortID returns the port with the given number.
func (s Status) PortID(id int) (Port, bool) {
	for _, p := range s.Ports {
		if p.ID == id {
			return p, true
		}
	}
	return Port{}, false
}

// PlanPoE returns the port as it should be after setting its PoE admin state.
func (s Status) PlanPoE(port int, on bool) (Port, error) {
	p, ok := s.PortID(port)
	if !ok {
		return Port{}, fmt.Errorf("no port %d", port)
	}
	if p.PoE == nil {
		return Port{}, fmt.Errorf("port %d has no PoE", port)
	}
	p.PoE = &on
	return p, nil
}

// PlanVLANs returns the port as it should be after setting its untagged VLAN
// (pvid) and tagged VLANs. Every VLAN must already exist on the switch.
func (s Status) PlanVLANs(port, pvid int, tagged []int) (Port, error) {
	p, ok := s.PortID(port)
	if !ok {
		return Port{}, fmt.Errorf("no port %d", port)
	}
	tagged = append([]int{}, slices.Compact(slices.Sorted(slices.Values(tagged)))...)
	for _, id := range append([]int{pvid}, tagged...) {
		if _, ok := s.VLAN(id); !ok {
			return Port{}, fmt.Errorf("no VLAN %d on the switch", id)
		}
	}
	if slices.Contains(tagged, pvid) {
		return Port{}, fmt.Errorf("VLAN %d can't be both untagged (PVID) and tagged", pvid)
	}
	p.PVID, p.Tagged = pvid, tagged
	return p, nil
}

// SameConfig reports whether two ports have the same label, PoE and VLAN
// settings. Link state is not configuration and is ignored.
func SameConfig(a, b Port) bool {
	samePoE := (a.PoE == nil) == (b.PoE == nil) && (a.PoE == nil || *a.PoE == *b.PoE)
	return a.ID == b.ID && a.Name == b.Name && samePoE && a.PVID == b.PVID && slices.Equal(a.Tagged, b.Tagged)
}

// Change is one port before and after a write.
type Change struct {
	Before Port `json:"before"`
	After  Port `json:"after"`
}

// Changed reports whether the write changed the port's configuration.
func (c Change) Changed() bool { return !SameConfig(c.Before, c.After) }

// Reader reads a switch's current state.
type Reader interface {
	Status(ctx context.Context) (Status, error)
}

// Controller changes a switch. Each method checks its result by reading the
// switch back, and returns an error if the port didn't end up as planned or
// another port's configuration changed.
type Controller interface {
	Reader
	SetPoE(ctx context.Context, port int, on bool) (Change, error)
	CyclePoE(ctx context.Context, port int) (Change, error)
	SetVLANs(ctx context.Context, port, pvid int, tagged []int) (Change, error)
}
