// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package ref

import (
	"errors"
	"strings"
	"testing"
)

func TestAppResolver(t *testing.T) {
	called := 0
	r := NewAppResolver([]string{"desk-form", "session"}, func(command, id string) ([]byte, error) {
		called++
		if command != "session" || id != "weekly-board" {
			t.Fatalf("runner got %q, %q", command, id)
		}
		return []byte(`{"name":"Weekly board","id":"weekly-board"}`), nil
	})
	g := NewRegistry()
	g.Register(App, r)
	if n, err := g.Resolve("app:session/weekly-board"); err != nil || n.Ref != "app:session/weekly-board" || n.Title != "Weekly board" || n.Open != "bashy session show weekly-board --json" {
		t.Fatalf("resolved node = %+v, %v", n, err)
	}
	if _, err := g.Resolve("app:unlisted/item"); !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "desk-form, session") {
		t.Fatalf("unknown command = %v", err)
	}
	if called != 1 {
		t.Fatalf("runner called %d times, want only the allowed command", called)
	}
}

func TestAppResolverPreservesRunnerError(t *testing.T) {
	denied := errors.New("session: permission denied for this board")
	r := NewAppResolver([]string{"session"}, func(_, _ string) ([]byte, error) { return nil, denied })
	if _, err := r.Resolve("session/board"); !errors.Is(err, denied) || err.Error() != denied.Error() {
		t.Fatalf("runner error changed: %v", err)
	}
	r = NewAppResolver([]string{"session"}, func(_, _ string) ([]byte, error) { return []byte("[]"), nil })
	if _, err := r.Resolve("session/board"); err == nil || !strings.Contains(err.Error(), "invalid JSON object") {
		t.Fatalf("invalid show output = %v", err)
	}
}
