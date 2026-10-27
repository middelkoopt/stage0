package ipmi

// sol is a session's Serial-over-LAN payload instance (step 3; for now
// activation is refused).
type sol struct{}

func (p *sol) receive([]byte) []byte { return nil }
func (p *sol) close()                {}

// payloadCommand answers Activate/Deactivate Payload and Get Payload
// Activation Status.
func (s *Server) payloadCommand(_ *session, m message) []byte {
	if m.cmd == cmdGetPayloadActivation {
		return m.response(ccOK, 1, 0, 0) // one instance, none active
	}
	return m.response(ccInvalidCommand)
}

// solConfig answers Get SOL Configuration Parameters.
func (s *Server) solConfig(m message) []byte {
	return m.response(ccInvalidCommand)
}
