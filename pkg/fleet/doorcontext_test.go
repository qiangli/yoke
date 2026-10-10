package fleet

import (
	"testing"

	"github.com/qiangli/yoke/pkg/broker/door"
)

// F17: a launch can ask the model door for a context window. The ask rides
// the door's own sticky-binding path — the URL form `bashy llm env --sticky`
// prints — so a base-URL-only client is bound without code changes.
func TestIsDoorBaseURL(t *testing.T) {
	t.Setenv(door.PortEnv, "")
	for in, want := range map[string]bool{
		"http://127.0.0.1:24556/v1":                    true,
		"http://127.0.0.1:24556":                       true,
		"http://127.0.0.1:24556/sticky/genie-opus5/v1": true,
		"http://127.0.0.1:24556/k/tok/s/s1/v1":         true,
		"http://localhost:24556/anthropic":             true,
		"http://[::1]:24556/v1":                        true,
		"https://127.0.0.1:24556/v1":                   false,
		"http://127.0.0.1:24556:80/v1":                 false,
		"https://api.example.test/v1":                  false,
		"http://127.0.0.1:11434/v1":                    false,
		"http://10.0.0.4:24556/v1":                     false,
		"127.0.0.1:24556":                              false,
		"":                                             false,
	} {
		if got := IsDoorBaseURL(in); got != want {
			t.Errorf("IsDoorBaseURL(%q) = %v, want %v", in, got, want)
		}
	}
	// The door's port is $BASHY_LLM_PORT when set.
	t.Setenv(door.PortEnv, "24560")
	if !IsDoorBaseURL("http://127.0.0.1:24560/v1") {
		t.Error("overridden door port not recognized")
	}
	if IsDoorBaseURL("http://127.0.0.1:24556/v1") {
		t.Error("default port accepted while overridden")
	}
}

func TestDoorContextBinding(t *testing.T) {
	t.Setenv(door.PortEnv, "")
	// A door URL with no sticky segment gains the context-asking binding,
	// keeping the rest of the path where it was.
	key, url, ok := DoorContextBinding("http://127.0.0.1:24556/v1", "qwen3:8b", 65536)
	if !ok || key != "minctx.qwen3-8b.65536" || url != "http://127.0.0.1:24556/sticky/minctx.qwen3-8b.65536/v1" {
		t.Fatalf("local = %q %q %v", key, url, ok)
	}
	// Existing /k and /s path parameters survive ahead of the binding.
	_, url, ok = DoorContextBinding("http://127.0.0.1:24556/k/tok/s/s1/v1", "m", 1000)
	if !ok || url != "http://127.0.0.1:24556/k/tok/s/s1/sticky/minctx.m.1000/v1" {
		t.Fatalf("params = %q %v", url, ok)
	}
	// A base URL already on a sticky binding is a frozen identity: not ours
	// to raise, so the door is not asked.
	_, _, ok = DoorContextBinding("http://127.0.0.1:24556/sticky/genie-opus5/v1", "claude-opus5", 65536)
	if ok {
		t.Fatal("frozen sticky identity re-bound")
	}
	// Not a door, no context asked, no model: none of these render.
	for _, tc := range []struct {
		base, model string
		min         int64
	}{
		{"https://api.example.test/v1", "m", 65536},
		{"http://127.0.0.1:24556/v1", "m", 0},
		{"http://127.0.0.1:24556/v1", "m", -1},
		{"", "m", 65536},
		{"http://127.0.0.1:24556/v1", "", 65536},
	} {
		if _, _, ok := DoorContextBinding(tc.base, tc.model, tc.min); ok {
			t.Fatalf("DoorContextBinding(%q, %q, %d) rendered", tc.base, tc.model, tc.min)
		}
	}
	// Model spellings outside a sticky key's alphabet fold into it.
	if key, _, ok := DoorContextBinding("http://127.0.0.1:24556/v1", "org/name:v2", 4096); !ok || key != "minctx.org-name-v2.4096" {
		t.Fatalf("key = %q %v", key, ok)
	}
}
