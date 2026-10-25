// Package netswitch is stage0's model of a managed network switch: its ports,
// their PoE state and their VLAN membership. Drivers for specific switches
// live in subpackages and return this model.
package netswitch

import "context"

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

// Reader reads a switch's current state.
type Reader interface {
	Status(ctx context.Context) (Status, error)
}
