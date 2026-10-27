package ipmi

import (
	"context"
	"net"
)

// Network functions (requests; a response is netFn+1) and commands.
const (
	netFnChassis   = 0x00
	netFnApp       = 0x06
	netFnTransport = 0x0c

	cmdGetChassisStatus = 0x01
	cmdChassisControl   = 0x02

	cmdGetDeviceID          = 0x01
	cmdGetChannelAuthCaps   = 0x38
	cmdSetSessionPrivilege  = 0x3b
	cmdCloseSession         = 0x3c
	cmdActivatePayload      = 0x48
	cmdDeactivatePayload    = 0x49
	cmdGetPayloadActivation = 0x4a
	cmdGetChannelCiphers    = 0x54

	cmdGetSOLConfig = 0x22

	ccOK             = 0x00
	ccInvalidCommand = 0xc1
	ccInvalidField   = 0xcc // also "invalid data"
	ccUnspecified    = 0xff
)

// message is an IPMI request (section 13.8).
type message struct {
	rqAddr, rqSeq, rqLUN byte
	netFn, cmd           byte
	data                 []byte
}

func parseMessage(b []byte) (message, bool) {
	// rsAddr, netFn/rsLUN, checksum, rqAddr, rqSeq/rqLUN, cmd, data, checksum
	if len(b) < 7 || checksum(b[:2]) != b[2] || checksum(b[3:len(b)-1]) != b[len(b)-1] {
		return message{}, false
	}
	return message{rqAddr: b[3], rqSeq: b[4] >> 2, rqLUN: b[4] & 3, netFn: b[1] >> 2, cmd: b[5], data: b[6 : len(b)-1]}, true
}

// response builds the answer to m.
func (m message) response(cc byte, data ...byte) []byte {
	r := []byte{m.rqAddr, (m.netFn+1)<<2 | m.rqLUN}
	r = append(r, checksum(r))
	body := append([]byte{0x20, m.rqSeq << 2, m.cmd, cc}, data...)
	r = append(r, body...)
	return append(r, checksum(body))
}

// checksum is the two's complement of the bytes' sum.
func checksum(b []byte) byte {
	var s byte
	for _, c := range b {
		s += c
	}
	return -s
}

// handle15 answers the one IPMI 1.5 packet a console sends before RMCP+:
// Get Channel Authentication Capabilities, without a session.
func (s *Server) handle15(ctx context.Context, b []byte) []byte {
	// auth type, sequence (4), session ID (4), length, message
	if len(b) < 10 || b[0] != 0 {
		return nil
	}
	n := int(b[9])
	if 10+n > len(b) {
		return nil
	}
	m, ok := parseMessage(b[10 : 10+n])
	if !ok {
		return nil
	}
	resp := s.sessionless(m)
	if resp == nil {
		return nil
	}
	p := append([]byte(nil), rmcpHeader...)
	p = append(p, 0, 0, 0, 0, 0, 0, 0, 0, 0, byte(len(resp)))
	return append(p, resp...)
}

// sessionless answers what may be asked outside a session.
func (s *Server) sessionless(m message) []byte {
	switch {
	case m.netFn == netFnApp && m.cmd == cmdGetChannelAuthCaps:
		// Channel 1 (LAN); auth types: IPMI 2.0 extended data; per-message
		// auth on, user-level off, non-null users only; IPMI 2.0
		// connections supported; no OEM.
		return m.response(ccOK, 0x01, 0x80, 0x14, 0x02, 0, 0, 0, 0)
	case m.netFn == netFnApp && m.cmd == cmdGetChannelCiphers:
		return m.response(ccOK, s.cipherSuites(m.data)...)
	}
	return nil
}

// cipherSuites answers Get Channel Cipher Suites: suites 3 and 17, as
// records, 16 bytes per list index.
func (s *Server) cipherSuites(req []byte) []byte {
	records := []byte{
		0xc0, 0x03, 0x01, 0x41, 0x81, // suite 3: RAKP-HMAC-SHA1, HMAC-SHA1-96, AES-CBC-128
		0xc0, 0x11, 0x03, 0x44, 0x81, // suite 17: RAKP-HMAC-SHA256, HMAC-SHA256-128, AES-CBC-128
	}
	index := 0
	if len(req) >= 3 {
		index = int(req[2] & 0x3f)
	}
	out := []byte{0x01}
	if start := index * 16; start < len(records) {
		out = append(out, records[start:min(start+16, len(records))]...)
	}
	return out
}

// command answers an IPMI message inside an established session.
func (s *Server) command(ctx context.Context, ss *session, b []byte) []byte {
	m, ok := parseMessage(b)
	if !ok {
		return nil
	}
	if r := s.sessionless(m); r != nil {
		return r
	}
	switch m.netFn {
	case netFnApp:
		switch m.cmd {
		case cmdGetDeviceID:
			// Device 0x20, revision 1, firmware 1.0, IPMI 2.0, chassis
			// device; manufacturer and product unassigned.
			return m.response(ccOK, 0x20, 0x01, 0x01, 0x00, 0x02, 0x80, 0, 0, 0, 0, 0)
		case cmdSetSessionPrivilege:
			level := byte(privAdmin)
			if len(m.data) > 0 && m.data[0] != 0 {
				level = m.data[0] & 0x0f
			}
			return m.response(ccOK, level)
		case cmdCloseSession:
			s.mu.Lock()
			ss.close()
			delete(s.sessions, ss.id)
			s.mu.Unlock()
			return m.response(ccOK)
		case cmdActivatePayload, cmdDeactivatePayload, cmdGetPayloadActivation:
			return s.payloadCommand(ss, m)
		}
	case netFnChassis:
		switch m.cmd {
		case cmdGetChassisStatus:
			on, err := s.Node.PowerOn(ctx)
			if err != nil {
				s.Log.Warn("ipmi: power state", "node", s.Name, "err", err)
				return m.response(ccUnspecified)
			}
			state := byte(0x00) // power off; restore policy "always off"
			if on {
				state |= 0x01
			}
			return m.response(ccOK, state, 0x00, 0x00)
		case cmdChassisControl:
			if len(m.data) < 1 {
				return m.response(ccInvalidField)
			}
			return m.response(s.chassisControl(ctx, m.data[0]&0x0f))
		}
	case netFnTransport:
		if m.cmd == cmdGetSOLConfig {
			return s.solConfig(m)
		}
	}
	return m.response(ccInvalidCommand)
}

// chassisControl accepts 0 power down, 1 up, 2 cycle, 3 hard reset (a
// cycle here) and 5 soft off (power down: a Pi has no ACPI), and carries
// it out in the background, in order, as a BMC does: a PoE change through
// the switch's web UI takes seconds, longer than a console waits.
func (s *Server) chassisControl(ctx context.Context, op byte) byte {
	steps := map[byte][]bool{0: {false}, 5: {false}, 1: {true}, 2: {false, true}, 3: {false, true}}[op]
	if steps == nil {
		return ccInvalidField
	}
	name := map[byte]string{0: "off", 1: "on", 2: "cycle", 3: "reset", 5: "soft off"}[op]
	s.Log.Info("ipmi: chassis control", "node", s.Name, "op", name)
	go func() {
		s.powerMu.Lock()
		defer s.powerMu.Unlock()
		for _, on := range steps {
			if err := s.Node.SetPower(context.WithoutCancel(ctx), on); err != nil {
				s.Log.Warn("ipmi: chassis control failed", "node", s.Name, "op", name, "err", err)
				return
			}
		}
	}()
	return ccOK
}

// sessionlessIPMI answers an IPMI message in an unauthenticated RMCP+
// packet with session ID 0 (consoles ask for cipher suites that way).
func (s *Server) sessionlessIPMI(payload []byte, _ net.Addr) []byte {
	m, ok := parseMessage(payload)
	if !ok {
		return nil
	}
	resp := s.sessionless(m)
	if resp == nil {
		return nil
	}
	return wrapPlain(payloadIPMI, resp)
}
