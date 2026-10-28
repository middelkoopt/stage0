package ipmi

import (
	"encoding/binary"
	"net"
	"sync"
	"time"
)

// SOL (Serial over LAN, spec section 15, 26): payload type 1 inside the
// session. Each packet: sequence (1–15; 0 for an ack only), the sequence
// it acknowledges, how many characters of it were accepted, a status
// byte, then characters. Output goes one packet at a time, resent until
// acknowledged.
const (
	solRetries  = 7
	solInterval = 500 * time.Millisecond
	solMaxData  = 200 // characters per packet
)

// sol is a session's active SOL payload: the node's console, bridged.
type sol struct {
	ss      *session
	console Console
	send    func([]byte) // sends a SOL payload to the console (wrapped)

	mu       sync.Mutex
	pending  []byte // console output not yet sent
	inFlight []byte
	seq      byte // of the packet in flight; 0 when none
	last     byte // the last sequence number used (1–15)
	tries    int
	wake     chan struct{}
	done     chan struct{}
	closed   bool
}

func newSOL(ss *session, console Console, send func([]byte)) *sol {
	p := &sol{ss: ss, console: console, send: send, wake: make(chan struct{}, 1), done: make(chan struct{})}
	go p.read()
	go p.run()
	return p
}

// read collects the console's output.
func (p *sol) read() {
	buf := make([]byte, 512)
	for {
		n, err := p.console.Read(buf)
		if n > 0 {
			p.mu.Lock()
			p.pending = append(p.pending, buf[:n]...)
			p.mu.Unlock()
			p.poke()
		}
		if err != nil {
			return
		}
	}
}

func (p *sol) poke() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// run sends output: one packet in flight, resent until acknowledged.
func (p *sol) run() {
	t := time.NewTicker(solInterval)
	defer t.Stop()
	for {
		select {
		case <-p.done:
			return
		case <-p.wake:
		case <-t.C:
		}
		p.mu.Lock()
		var out []byte
		if p.seq != 0 {
			if p.tries < solRetries { // resend: not acknowledged yet
				p.tries++
				out = p.packet()
			} else { // given up on it
				p.seq, p.inFlight = 0, nil
			}
		}
		if p.seq == 0 && len(p.pending) > 0 {
			n := min(len(p.pending), solMaxData)
			p.inFlight = append([]byte(nil), p.pending[:n]...)
			p.pending = p.pending[n:]
			p.last = p.last%15 + 1
			p.seq, p.tries = p.last, 1
			out = p.packet()
		}
		p.mu.Unlock()
		if out != nil {
			p.send(out)
		}
	}
}

// packet is the in-flight output (p.mu held).
func (p *sol) packet() []byte {
	return append([]byte{p.seq, 0, 0, 0}, p.inFlight...)
}

// receive takes a SOL packet from the console: its acknowledgement of our
// output, and characters to type. It returns the ack to send, if any.
func (p *sol) receive(b []byte) []byte {
	if len(b) < 4 {
		return nil
	}
	seq, ack := b[0]&0x0f, b[1]&0x0f
	data := b[4:]
	p.mu.Lock()
	if ack != 0 && ack == p.seq {
		p.seq, p.inFlight = 0, nil
	}
	p.mu.Unlock()
	p.poke()
	if seq == 0 {
		return nil // an ack only
	}
	if len(data) > 0 {
		p.console.Write(data)
	}
	return []byte{0, seq, byte(len(data)), 0}
}

func (p *sol) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	close(p.done)
	p.console.Close()
}

// payloadCommand answers Activate/Deactivate Payload and Get Payload
// Activation Status (SOL, instance 1).
func (s *Server) payloadCommand(ss *session, m message) []byte {
	if len(m.data) < 1 || m.data[0]&0x3f != payloadSOL {
		return m.response(ccInvalidField)
	}
	switch m.cmd {
	case cmdGetPayloadActivation:
		active := byte(0)
		if s.solActive() {
			active = 1
		}
		return m.response(ccOK, 1, active, 0)
	case cmdActivatePayload:
		if s.solActive() {
			return m.response(0x80) // payload already active
		}
		console, err := s.Node.Console()
		if err != nil {
			s.Log.Warn("ipmi: sol", "node", s.Name, "err", err)
			return m.response(ccUnspecified)
		}
		s.mu.Lock()
		from, conn := ss.from, s.conn
		s.mu.Unlock()
		p := newSOL(ss, console, func(b []byte) {
			if _, err := conn.WriteTo(ss.wrap(payloadSOL, b), from); err != nil {
				s.Log.Debug("ipmi: sol send", "err", err)
			}
		})
		s.mu.Lock()
		ss.sol = p
		s.mu.Unlock()
		s.Log.Info("ipmi: sol activated", "node", s.Name, "from", from.String())
		// Aux data, inbound and outbound payload sizes, the UDP port, no VLAN.
		r := []byte{0, 0, 0, 0}
		r = binary.LittleEndian.AppendUint16(r, solMaxData)
		r = binary.LittleEndian.AppendUint16(r, solMaxData)
		r = binary.LittleEndian.AppendUint16(r, s.port())
		r = append(r, 0xff, 0xff)
		return m.response(ccOK, r...)
	case cmdDeactivatePayload:
		s.mu.Lock()
		p := ss.sol
		ss.sol = nil
		s.mu.Unlock()
		if p == nil {
			return m.response(0x80) // payload already deactivated
		}
		p.close()
		s.Log.Info("ipmi: sol deactivated", "node", s.Name)
		return m.response(ccOK)
	}
	return m.response(ccInvalidCommand)
}

// solActive reports whether any session has SOL active (one instance).
func (s *Server) solActive() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ss := range s.sessions {
		if ss.sol != nil {
			return true
		}
	}
	return false
}

func (s *Server) port() uint16 {
	if u, ok := s.conn.LocalAddr().(*net.UDPAddr); ok {
		return uint16(u.Port)
	}
	return 623
}

// solConfig answers Get SOL Configuration Parameters: SOL enabled, at
// 115200 baud, on this channel and port.
func (s *Server) solConfig(m message) []byte {
	if len(m.data) < 2 {
		return m.response(ccInvalidField)
	}
	const rev = 0x11
	if m.data[0]&0x80 != 0 { // revision only
		return m.response(ccOK, rev)
	}
	switch m.data[1] {
	case 0x00: // set in progress
		return m.response(ccOK, rev, 0x00)
	case 0x01: // SOL enable
		return m.response(ccOK, rev, 0x01)
	case 0x02: // authentication: user privilege
		return m.response(ccOK, rev, 0x02)
	case 0x03: // character accumulate interval (5 ms units), send threshold
		return m.response(ccOK, rev, 0x0c, 0x60)
	case 0x04: // retry count, interval (10 ms units)
		return m.response(ccOK, rev, solRetries, byte(solInterval/(10*time.Millisecond)))
	case 0x05, 0x06: // non-volatile, volatile bit rate: 115200
		return m.response(ccOK, rev, 0x0a)
	case 0x07: // payload channel
		return m.response(ccOK, rev, 0x01)
	case 0x08: // payload port
		return m.response(ccOK, append([]byte{rev}, binary.LittleEndian.AppendUint16(nil, s.port())...)...)
	}
	return m.response(0x80) // parameter not supported
}
