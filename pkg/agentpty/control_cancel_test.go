package agentpty

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestControlWriteCancellationJoinsBlockedWrite(t *testing.T) {
	writer, reader := net.Pipe()
	defer writer.Close()
	defer reader.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- writeControlFrame(ctx, writer, "pending frame") }()
	// net.Pipe cannot complete the write until the entire frame is read.
	// Reading one byte proves the write started while leaving it blocked.
	var first [1]byte
	if _, err := reader.Read(first[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Fatal("write finished before frame was consumed")
	default:
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled write succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not release blocked write")
	}
}
