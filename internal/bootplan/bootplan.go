// Package bootplan is the agent's install plan as kernel parameters
// (stage0.disk=, stage0.url=, …): stage0 encodes it into the command line
// it boots the agent with, and the agent parses it back. The names live
// only here.
package bootplan

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Plan is what the agent is to do. Empty fields are left out.
type Plan struct {
	Disk   string // the target under /dev, e.g. mmcblk0
	SHA256 string // of the disk image, or of a container's manifest
	URL    string // the disk image; empty: it is in the initramfs
	Seed   string // first-boot files for the boot partition
	OCI    string // instead of URL: a container's OCI layout
	Layout string // with OCI: how to partition (JSON, a container.Spec)
	Report string // where to POST reports
}

// The parameters, in the order Encode writes them.
var params = []struct {
	name  string
	field func(*Plan) *string
}{
	{"stage0.disk", func(p *Plan) *string { return &p.Disk }},
	{"stage0.sha256", func(p *Plan) *string { return &p.SHA256 }},
	{"stage0.url", func(p *Plan) *string { return &p.URL }},
	{"stage0.oci", func(p *Plan) *string { return &p.OCI }},
	{"stage0.layout", func(p *Plan) *string { return &p.Layout }},
	{"stage0.seed", func(p *Plan) *string { return &p.Seed }},
	{"stage0.report", func(p *Plan) *string { return &p.Report }},
}

// Encode returns the plan as kernel parameters, space-separated.
func (p Plan) Encode() string {
	var out []string
	for _, k := range params {
		if v := *k.field(&p); v != "" {
			out = append(out, k.name+"="+v)
		}
	}
	return strings.Join(out, " ")
}

var (
	diskName = regexp.MustCompile(`^[a-z][a-z0-9]*$`)
	hexSum   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Parse reads a plan from a kernel command line, checking it.
func Parse(cmdline string) (Plan, error) {
	var p Plan
	for _, f := range strings.Fields(cmdline) {
		for _, k := range params {
			if v, ok := strings.CutPrefix(f, k.name+"="); ok {
				*k.field(&p) = v
			}
		}
	}
	for _, u := range []struct{ name, v string }{
		{"stage0.url", p.URL}, {"stage0.seed", p.Seed}, {"stage0.oci", p.OCI},
		{"stage0.layout", p.Layout}, {"stage0.report", p.Report},
	} {
		if u.v != "" && !strings.HasPrefix(u.v, "http://") {
			return p, fmt.Errorf("%s=%q: only http:// is supported", u.name, u.v)
		}
	}
	switch {
	case p.Disk == "" && p.SHA256 == "":
		return p, nil // nothing to install
	case p.Disk == "":
		return p, errors.New("stage0.sha256 is set but stage0.disk is not")
	case p.SHA256 == "":
		return p, errors.New("stage0.disk is set but stage0.sha256 is not")
	case !diskName.MatchString(p.Disk):
		return p, fmt.Errorf("stage0.disk=%q is not a disk name like mmcblk0", p.Disk)
	case !hexSum.MatchString(p.SHA256):
		return p, fmt.Errorf("stage0.sha256=%q is not 64 hex digits", p.SHA256)
	case p.OCI != "" && p.URL != "":
		return p, errors.New("stage0.oci and stage0.url: an image or a container, not both")
	case (p.OCI != "") != (p.Layout != ""):
		return p, errors.New("stage0.oci and stage0.layout go together")
	}
	return p, nil
}

// Installs reports whether the plan writes a disk.
func (p Plan) Installs() bool { return p.Disk != "" }

// NeedsNetwork reports whether the plan fetches or reports anything.
func (p Plan) NeedsNetwork() bool {
	return p.URL != "" || p.Seed != "" || p.OCI != "" || p.Report != ""
}

// IPv6 reports whether the plan's server is an IPv6 address
// (http://[…]/): the agent then takes an address by SLAAC.
func (p Plan) IPv6() bool {
	for _, u := range []string{p.URL, p.OCI, p.Report} {
		if u != "" {
			return strings.HasPrefix(u, "http://[")
		}
	}
	return false
}
