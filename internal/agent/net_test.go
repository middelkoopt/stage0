package agent

import (
	"bytes"
	"net/netip"
	"testing"
)

func TestNewAddrMessage(t *testing.T) {
	got := newAddrMessage(3, netip.MustParsePrefix("10.0.9.100/24"), 7)
	want := []byte{
		40, 0, 0, 0, // length 40
		20, 0, // RTM_NEWADDR
		0x05, 0x06, // NLM_F_REQUEST|NLM_F_ACK|NLM_F_EXCL|NLM_F_CREATE
		7, 0, 0, 0, // sequence
		0, 0, 0, 0, // port id: the kernel picks
		2, 24, 0, 0, // AF_INET, /24, flags, scope universe
		3, 0, 0, 0, // interface index
		8, 0, 2, 0, 10, 0, 9, 100, // IFA_LOCAL
		8, 0, 1, 0, 10, 0, 9, 100, // IFA_ADDRESS
	}
	if !bytes.Equal(got, want) {
		t.Errorf("message\n got % x\nwant % x", got, want)
	}
}
