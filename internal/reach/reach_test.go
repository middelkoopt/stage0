package reach

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func listen(t *testing.T, greeting string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte(greeting))
			c.Close()
		}
	}()
	return ln.Addr().String()
}

func TestSSH(t *testing.T) {
	addr := listen(t, "SSH-2.0-OpenSSH_9.9\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	v, err := sshGreeting(ctx, addr)
	if err != nil || v != "SSH-2.0-OpenSSH_9.9" {
		t.Errorf("greeting %q, %v", v, err)
	}
}

func TestSSHNotSSH(t *testing.T) {
	addr := listen(t, "HTTP/1.1 400 Bad Request\r\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sshGreeting(ctx, addr); err == nil || !strings.Contains(err.Error(), "not an SSH server") {
		t.Errorf("err = %v", err)
	}
}

func TestPingLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := Ping(ctx, "127.0.0.1"); err != nil {
		t.Skipf("unprivileged ping not allowed here: %v", err)
	}
}

func TestCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// 192.0.2.1 is reserved for documentation (RFC 5737): nothing answers.
	r := Check(ctx, "192.0.2.1")
	if r.OK() || r.Ping || r.Err == "" {
		t.Errorf("check of an address nothing answers on: %+v", r)
	}
}
