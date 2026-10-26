// Package reach checks that an installed node is reachable: it answers a
// ping and its SSH server greets. No key is needed, and no root where
// net.ipv4.ping_group_range allows unprivileged ICMP sockets.
package reach

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// Result is one check of an address.
type Result struct {
	Address string `json:"address"`
	Ping    bool   `json:"ping"`
	SSH     string `json:"ssh,omitempty"`   // the server's greeting, e.g. SSH-2.0-OpenSSH_9.9
	Err     string `json:"error,omitempty"` // why it isn't reachable
}

// OK reports whether the address answered both.
func (r Result) OK() bool { return r.Ping && r.SSH != "" }

// Check pings addr (an IP, with %zone for link-local) and reads its SSH
// greeting.
func Check(ctx context.Context, addr string) Result {
	r := Result{Address: addr}
	if err := Ping(ctx, addr); err != nil {
		r.Err = err.Error()
		return r
	}
	r.Ping = true
	v, err := sshGreeting(ctx, net.JoinHostPort(addr, "22"))
	if err != nil {
		r.Err = err.Error()
		return r
	}
	r.SSH = v
	return r
}

// Wait checks addr every few seconds until it is reachable or ctx is done,
// and returns the last result.
func Wait(ctx context.Context, addr string) Result {
	for {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		r := Check(cctx, addr)
		cancel()
		if r.OK() {
			return r
		}
		select {
		case <-ctx.Done():
			return r
		case <-time.After(3 * time.Second):
		}
	}
}

// Ping sends one ICMP echo to addr and waits for the reply.
func Ping(ctx context.Context, addr string) error {
	host, zone, _ := strings.Cut(addr, "%")
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	network, laddr := "udp4", "0.0.0.0"
	var typ icmp.Type = ipv4.ICMPTypeEcho
	proto := 1
	if ip.Is6() {
		network, laddr, typ, proto = "udp6", "::", ipv6.ICMPTypeEchoRequest, 58
		if zone != "" {
			laddr += "%" + zone
		}
	}
	c, err := icmp.ListenPacket(network, laddr)
	if err != nil {
		return fmt.Errorf("ping: ICMP socket (is net.ipv4.ping_group_range open?): %w", err)
	}
	defer c.Close()
	m := icmp.Message{Type: typ, Body: &icmp.Echo{ID: 1, Seq: 1, Data: []byte("stage0")}}
	b, err := m.Marshal(nil)
	if err != nil {
		return err
	}
	if _, err := c.WriteTo(b, &net.UDPAddr{IP: ip.AsSlice(), Zone: zone}); err != nil {
		return fmt.Errorf("ping %s: %w", addr, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetReadDeadline(deadline)
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return fmt.Errorf("ping %s: no reply", addr)
		}
		u, ok := from.(*net.UDPAddr)
		if !ok || !u.IP.Equal(ip.AsSlice()) {
			continue
		}
		if r, err := icmp.ParseMessage(proto, buf[:n]); err == nil &&
			(r.Type == ipv4.ICMPTypeEchoReply || r.Type == ipv6.ICMPTypeEchoReply) {
			return nil
		}
	}
}

// sshGreeting connects to addr and returns the server's identification
// line (RFC 4253 4.2).
func sshGreeting(ctx context.Context, addr string) (string, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return "", fmt.Errorf("ssh %s: %w", addr, err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	line, err := bufio.NewReader(c).ReadString('\n')
	line = strings.TrimRight(line, "\r\n")
	switch {
	case strings.HasPrefix(line, "SSH-"):
		return line, nil
	case err != nil:
		return "", fmt.Errorf("ssh %s: %w", addr, err)
	}
	return "", errors.New("ssh " + addr + ": not an SSH server: " + fmt.Sprintf("%.40q", line))
}
