package ipmi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"hash"
	"net"
	"time"
)

const (
	authRMCPPlus = 0x06 // the IPMI 2.0 session header's "auth type"

	payloadIPMI         = 0x00
	payloadSOL          = 0x01
	payloadOpenRequest  = 0x10
	payloadOpenResponse = 0x11
	payloadRAKP1        = 0x12
	payloadRAKP2        = 0x13
	payloadRAKP3        = 0x14
	payloadRAKP4        = 0x15

	flagEncrypted     = 0x80
	flagAuthenticated = 0x40

	// RMCP+ status codes (spec table 13-15).
	statusOK                    = 0x00
	statusNoResources           = 0x01
	statusInvalidSessionID      = 0x02
	statusInvalidRole           = 0x09
	statusUnauthorizedName      = 0x0d
	statusInvalidAuthAlg        = 0x11
	statusInvalidIntegrity      = 0x12
	statusInvalidConf           = 0x13
	statusInvalidIntegrityCheck = 0x0f

	privAdmin = 0x04
)

// suite is a cipher suite's algorithms.
type suite struct {
	auth, integrity, conf byte
	hash                  func() hash.Hash
	icvLen                int // integrity check value: truncated HMAC
}

// suites by authentication algorithm (1 RAKP-HMAC-SHA1, 3 RAKP-HMAC-SHA256);
// integrity (1 HMAC-SHA1-96, 4 HMAC-SHA256-128) and confidentiality (1
// AES-CBC-128) follow, as in suites 3 and 17.
var suites = map[byte]suite{
	1: {auth: 1, integrity: 1, conf: 1, hash: sha1.New, icvLen: 12},
	3: {auth: 3, integrity: 4, conf: 1, hash: sha256.New, icvLen: 16},
}

// session is one RMCP+ session.
type session struct {
	id, remoteID uint32
	suite        suite
	rm, rc       [16]byte
	role         byte
	user         []byte
	established  bool
	k1, k2       []byte
	last         time.Time
	from         net.Addr
	sol          *sol

	// The last request (decrypted) and its response, for resends.
	lastReq, lastResp []byte
	lastAt            time.Time
}

// resendWindow is how long a repeated request counts as a resend.
const resendWindow = 30 * time.Second

func (ss *session) close() {
	if ss.sol != nil {
		ss.sol.close()
		ss.sol = nil
	}
}

func (ss *session) mac(key []byte, parts ...[]byte) []byte {
	m := hmac.New(ss.suite.hash, key)
	for _, p := range parts {
		m.Write(p)
	}
	return m.Sum(nil)
}

// kuid is the user's key: the password, zero-padded to 20 bytes.
func (s *Server) kuid() []byte {
	k := make([]byte, 20)
	copy(k, s.Password)
	return k
}

func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

// handle20 answers an IPMI 2.0 (RMCP+) session packet.
func (s *Server) handle20(ctx context.Context, b []byte, from net.Addr) []byte {
	// auth type, payload type, session ID, sequence, length, payload.
	if len(b) < 12 {
		return nil
	}
	ptype := b[1] & 0x3f
	flags := b[1] & 0xc0
	sid := binary.LittleEndian.Uint32(b[2:])
	n := int(binary.LittleEndian.Uint16(b[10:]))
	if 12+n > len(b) {
		return nil
	}
	payload := b[12 : 12+n]
	switch ptype {
	case payloadOpenRequest:
		return s.openSession(payload)
	case payloadRAKP1:
		return s.rakp1(payload, from)
	case payloadRAKP3:
		return s.rakp3(payload)
	}
	if sid == 0 && ptype == payloadIPMI && flags == 0 {
		return s.sessionlessIPMI(payload, from)
	}
	s.mu.Lock()
	ss := s.sessions[sid]
	s.mu.Unlock()
	if ss == nil || !ss.established {
		return nil
	}
	// Integrity: HMAC(K1) over everything from the auth type through the
	// next-header byte, then the truncated HMAC.
	if flags&flagAuthenticated == 0 {
		return nil
	}
	tail := b[12+n:]
	if len(tail) < 2+ss.suite.icvLen {
		return nil
	}
	icvAt := len(b) - ss.suite.icvLen
	want := ss.mac(ss.k1, b[:icvAt])[:ss.suite.icvLen]
	if subtle.ConstantTimeCompare(want, b[icvAt:]) != 1 {
		s.Log.Debug("ipmi: bad integrity check", "node", s.Name)
		return nil
	}
	if flags&flagEncrypted != 0 {
		var ok bool
		if payload, ok = ss.decrypt(payload); !ok {
			return nil
		}
	}
	s.mu.Lock()
	ss.last = time.Now()
	ss.from = from
	s.mu.Unlock()
	switch ptype {
	case payloadIPMI:
		// A console resends a request it got no answer to in time (a PoE
		// change takes seconds): answer a resend from the cache, so the
		// command runs once.
		if bytes.Equal(payload, ss.lastReq) && time.Since(ss.lastAt) < resendWindow {
			return ss.wrap(payloadIPMI, ss.lastResp)
		}
		resp := s.command(ctx, ss, payload)
		if resp == nil {
			return nil
		}
		ss.lastReq, ss.lastResp, ss.lastAt = append([]byte(nil), payload...), resp, time.Now()
		return ss.wrap(payloadIPMI, resp)
	case payloadSOL:
		s.mu.Lock()
		p := ss.sol
		s.mu.Unlock()
		if p != nil {
			if out := p.receive(payload); out != nil {
				return ss.wrap(payloadSOL, out)
			}
		}
	}
	return nil
}

// wrap builds an encrypted, authenticated RMCP+ packet for the session.
func (ss *session) wrap(ptype byte, payload []byte) []byte {
	enc := ss.encrypt(payload)
	p := append([]byte(nil), rmcpHeader...)
	p = append(p, authRMCPPlus, ptype|flagEncrypted|flagAuthenticated)
	p = append(p, le32(ss.remoteID)...)
	p = append(p, 0, 0, 0, 0) // session sequence: unused by consoles here
	p = binary.LittleEndian.AppendUint16(p, uint16(len(enc)))
	p = append(p, enc...)
	// Integrity pad: from the auth type through the pad length and next
	// header, a multiple of 4.
	padLen := (4 - (len(p)-4+2)%4) % 4
	for range padLen {
		p = append(p, 0xff)
	}
	p = append(p, byte(padLen), 0x07)
	return append(p, ss.mac(ss.k1, p[4:])[:ss.suite.icvLen]...)
}

// encrypt is AES-CBC-128 with K2: a random IV, then the payload padded
// with 1, 2, 3… and the pad length to a whole block.
func (ss *session) encrypt(b []byte) []byte {
	block, _ := aes.NewCipher(ss.k2[:16])
	pad := (16 - (len(b)+1)%16) % 16
	data := append([]byte(nil), b...)
	for i := range pad {
		data = append(data, byte(i+1))
	}
	data = append(data, byte(pad))
	out := make([]byte, 16+len(data))
	rand.Read(out[:16])
	cipher.NewCBCEncrypter(block, out[:16]).CryptBlocks(out[16:], data)
	return out
}

func (ss *session) decrypt(b []byte) ([]byte, bool) {
	if len(b) < 32 || len(b)%16 != 0 {
		return nil, false
	}
	block, _ := aes.NewCipher(ss.k2[:16])
	data := make([]byte, len(b)-16)
	cipher.NewCBCDecrypter(block, b[:16]).CryptBlocks(data, b[16:])
	pad := int(data[len(data)-1])
	if pad+1 > len(data) {
		return nil, false
	}
	return data[:len(data)-1-pad], true
}

// openSession answers an RMCP+ Open Session Request.
func (s *Server) openSession(p []byte) []byte {
	if len(p) < 32 {
		return nil
	}
	tag, role, remoteID := p[0], p[1], binary.LittleEndian.Uint32(p[4:])
	auth, integ, conf := p[8+4], p[16+4], p[24+4]
	status := byte(statusOK)
	st, ok := suites[auth]
	switch {
	case !ok:
		status = statusInvalidAuthAlg
	case integ != st.integrity:
		status = statusInvalidIntegrity
	case conf != st.conf:
		status = statusInvalidConf
	}
	resp := []byte{tag, status, privAdmin, 0}
	resp = append(resp, le32(remoteID)...)
	if status != statusOK {
		return wrapPlain(payloadOpenResponse, append(resp, make([]byte, 4)...))
	}
	var idb [4]byte
	rand.Read(idb[:])
	id := binary.LittleEndian.Uint32(idb[:]) | 1 // never 0, the session-less ID
	_ = role
	ss := &session{id: id, remoteID: remoteID, suite: st, last: time.Now()}
	s.mu.Lock()
	if len(s.sessions) >= 8 {
		s.mu.Unlock()
		resp[1] = statusNoResources
		return wrapPlain(payloadOpenResponse, append(resp, make([]byte, 4)...))
	}
	s.sessions[id] = ss
	s.mu.Unlock()
	resp = append(resp, le32(id)...)
	resp = append(resp, 0, 0, 0, 8, st.auth, 0, 0, 0)
	resp = append(resp, 1, 0, 0, 8, st.integrity, 0, 0, 0)
	resp = append(resp, 2, 0, 0, 8, st.conf, 0, 0, 0)
	return wrapPlain(payloadOpenResponse, resp)
}

// rakp1 answers RAKP Message 1 with Message 2.
func (s *Server) rakp1(p []byte, from net.Addr) []byte {
	if len(p) < 28 {
		return nil
	}
	tag, id := p[0], binary.LittleEndian.Uint32(p[4:])
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil {
		return wrapPlain(payloadRAKP2, []byte{tag, statusInvalidSessionID, 0, 0, 0, 0, 0, 0})
	}
	copy(ss.rm[:], p[8:24])
	ss.role = p[24]
	ulen := int(p[27])
	if 28+ulen > len(p) {
		return nil
	}
	ss.user = append([]byte(nil), p[28:28+ulen]...)
	resp := []byte{tag, statusOK, 0, 0}
	resp = append(resp, le32(ss.remoteID)...)
	if string(ss.user) != s.User {
		resp[1] = statusUnauthorizedName
		return wrapPlain(payloadRAKP2, resp)
	}
	if ss.role&0x0f > privAdmin {
		resp[1] = statusInvalidRole
		return wrapPlain(payloadRAKP2, resp)
	}
	rand.Read(ss.rc[:])
	ss.from = from
	resp = append(resp, ss.rc[:]...)
	resp = append(resp, s.guid[:]...)
	resp = append(resp, ss.mac(s.kuid(), le32(ss.remoteID), le32(ss.id), ss.rm[:], ss.rc[:], s.guid[:],
		[]byte{ss.role, byte(len(ss.user))}, ss.user)...)
	return wrapPlain(payloadRAKP2, resp)
}

// rakp3 checks RAKP Message 3 and answers with Message 4, establishing
// the session and its keys.
func (s *Server) rakp3(p []byte) []byte {
	if len(p) < 8 {
		return nil
	}
	tag, status, id := p[0], p[1], binary.LittleEndian.Uint32(p[4:])
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil {
		return wrapPlain(payloadRAKP4, []byte{tag, statusInvalidSessionID, 0, 0, 0, 0, 0, 0})
	}
	drop := func(code byte) []byte {
		s.mu.Lock()
		delete(s.sessions, id)
		s.mu.Unlock()
		return wrapPlain(payloadRAKP4, append([]byte{tag, code, 0, 0}, le32(ss.remoteID)...))
	}
	if status != statusOK {
		return drop(status)
	}
	want := ss.mac(s.kuid(), ss.rc[:], le32(ss.remoteID), []byte{ss.role, byte(len(ss.user))}, ss.user)
	if subtle.ConstantTimeCompare(want, p[8:]) != 1 {
		s.Log.Info("ipmi: wrong password", "node", s.Name, "user", string(ss.user))
		return drop(statusInvalidIntegrityCheck)
	}
	sik := ss.mac(s.kuid(), ss.rm[:], ss.rc[:], []byte{ss.role, byte(len(ss.user))}, ss.user)
	// K1 and K2: HMAC(SIK) of 20 bytes of 0x01 and 0x02, for SHA-256 too
	// (as ipmitool derives them).
	ss.k1 = ss.mac(sik, constant(0x01, 20))
	ss.k2 = ss.mac(sik, constant(0x02, 20))
	ss.established = true
	resp := append([]byte{tag, statusOK, 0, 0}, le32(ss.remoteID)...)
	resp = append(resp, ss.mac(sik, ss.rm[:], le32(ss.id), s.guid[:])[:ss.suite.icvLen]...)
	s.Log.Info("ipmi: session", "node", s.Name, "user", string(ss.user), "from", ss.from.String())
	return wrapPlain(payloadRAKP4, resp)
}

func constant(b byte, n int) []byte {
	c := make([]byte, n)
	for i := range c {
		c[i] = b
	}
	return c
}

// wrapPlain builds an RMCP+ packet outside a session (session ID 0).
func wrapPlain(ptype byte, payload []byte) []byte {
	p := append([]byte(nil), rmcpHeader...)
	p = append(p, authRMCPPlus, ptype, 0, 0, 0, 0, 0, 0, 0, 0)
	p = binary.LittleEndian.AppendUint16(p, uint16(len(payload)))
	return append(p, payload...)
}
