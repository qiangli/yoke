package secrets

import (
	"strings"
	"sync"
	"testing"
	"time"
)

type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitFor(t *testing.T, b *syncBuf, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if b.String() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("got %q, want %q", b.String(), want)
}

func TestLiveWriterFlushesShortLineWhenIdle(t *testing.T) {
	r := NewRedactor()
	buf := &syncBuf{}
	w := r.LiveWriter(buf)
	defer w.Close()
	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, buf, "hello\n")
}

func TestLiveWriterSplitSecretStillMasked(t *testing.T) {
	r := NewRedactor()
	if err := r.Register("tok", "s3cr3t-value-123456"); err != nil {
		t.Fatal(err)
	}
	buf := &syncBuf{}
	w := r.LiveWriter(buf)
	w.Write([]byte("x s3cr3t-val"))
	w.Write([]byte("ue-123456 y\n"))
	w.Close()
	if got := buf.String(); strings.Contains(got, "s3cr3t") || !strings.Contains(got, "y\n") {
		t.Fatalf("got %q", got)
	}
}
