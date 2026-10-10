package meet

import (
	"context"
	"errors"
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

// Only an explicit refusal may clear the port. The wrapped shape is what
// net.Dialer returns; every other failure stays fail-closed.
func TestIsConnRefusedMatchesOnlyThePlatformRefusal(t *testing.T) {
	wrap := func(err error) error {
		return &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connectex", err)}
	}
	if !isConnRefused(wrap(platformRefusal)) {
		t.Fatalf("the platform refusal %v must classify as refused", platformRefusal)
	}
	for name, err := range map[string]error{
		"timeout":     wrap(context.DeadlineExceeded),
		"permission":  wrap(syscall.EACCES),
		"unreachable": wrap(syscall.ENETUNREACH),
		"reset":       wrap(syscall.ECONNRESET),
		"unknown":     errors.New("connection refused"), // prose is not evidence
		"nil":         nil,
	} {
		if isConnRefused(err) {
			t.Errorf("%s (%v) must not classify as refused", name, err)
		}
	}
}

// A real dial to a just-closed loopback port must produce the error the
// classifier recognizes — the native-Windows failure was exactly this pair
// disagreeing.
func TestIsConnRefusedRecognizesARealClosedPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		_ = conn.Close()
		t.Skip("the closed port was reused before the dial")
	}
	if !isConnRefused(err) {
		t.Fatalf("dial to a closed port returned %#v, which isConnRefused does not recognize", err)
	}
}
