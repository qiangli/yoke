package chat

import (
	"context"
	"errors"
	"github.com/qiangli/yoke/pkg/acp"
	"sync/atomic"
	"testing"
	"time"
)

type blockingACPWriter struct {
	entered, release chan struct{}
	writes           atomic.Int32
}

func (w *blockingACPWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	close(w.entered)
	<-w.release
	return len(p), nil
}

func TestACPCloseJoinsInflightCallbackAndRejectsQueuedUpdate(t *testing.T) {
	w := &blockingACPWriter{entered: make(chan struct{}), release: make(chan struct{})}
	d := &acpDriver{client: &acp.Client{}, sink: w}
	d.lifetime.init(context.Background())
	s := &Session{acp: d, done: make(chan struct{})}
	h := &acpHandler{d: d}
	update := acp.SessionNotification{Update: acp.SessionUpdate{AgentMessageChunk: &acp.ContentBlock{Text: "hello"}}}
	updated := make(chan error, 1)
	go func() { updated <- h.SessionUpdate(context.Background(), update) }()
	<-w.entered
	stopped := make(chan struct{})
	go func() { s.Close(); close(stopped) }()
	<-d.lifetime.ctx.Done()
	select {
	case <-stopped:
		t.Fatal("close abandoned active callback")
	default:
	}
	select {
	case <-s.done:
		t.Fatal("session cleanup completed before callback")
	default:
	}
	close(w.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("close did not join callback")
	}
	if err := <-updated; err != nil {
		t.Fatal(err)
	}
	if err := h.SessionUpdate(context.Background(), update); !errors.Is(err, context.Canceled) {
		t.Fatalf("late update: %v", err)
	}
	if w.writes.Load() != 1 {
		t.Fatal("late callback wrote after close")
	}
	if err := d.prompt("late prompt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("late prompt: %v", err)
	}
}

func TestACPLifetimeCancelsAndJoinsAdmittedWork(t *testing.T) {
	var l acpLifetime
	ctx, leave := l.enter(context.Background())
	l.stop()
	<-ctx.Done()
	done := make(chan struct{})
	go func() { l.wait(); close(done) }()
	select {
	case <-done:
		t.Fatal("join abandoned admitted work")
	default:
	}
	leave()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("join did not finish")
	}
	if _, leave := l.enter(context.Background()); leave != nil {
		t.Fatal("admitted work after stop")
	}
}

type waitingACPApproval struct {
	acp.BaseHandler
	entered, cancelled, release chan struct{}
}

func (a *waitingACPApproval) RequestPermission(ctx context.Context, _ acp.PermissionRequest) (acp.PermissionResponse, error) {
	close(a.entered)
	<-ctx.Done()
	close(a.cancelled)
	<-a.release
	return acp.PermissionResponse{Cancelled: true}, ctx.Err()
}

func TestACPCloseCancelsAndJoinsPermissionCallback(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	a := &waitingACPApproval{entered: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	d := &acpDriver{client: &acp.Client{}}
	d.lifetime.init(context.Background())
	s := &Session{acp: d, done: make(chan struct{})}
	h := &acpHandler{d: d, approvals: a}
	result := make(chan error, 1)
	go func() { _, err := h.RequestPermission(context.Background(), acp.PermissionRequest{}); result <- err }()
	<-a.entered
	stopped := make(chan struct{})
	go func() { s.Close(); close(stopped) }()
	select {
	case <-a.cancelled:
	case <-time.After(time.Second):
		t.Fatal("permission callback was not cancelled")
	}
	select {
	case <-stopped:
		t.Fatal("close abandoned permission callback")
	default:
	}
	close(a.release)
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("close did not join permission callback")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("permission error: %v", err)
	}
	if _, err := h.RequestPermission(context.Background(), acp.PermissionRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("late permission error: %v", err)
	}
}
