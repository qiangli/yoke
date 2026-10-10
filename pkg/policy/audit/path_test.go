// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package audit

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultPathLadder(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	for _, v := range []string{"", "off", "1"} {
		t.Setenv("BASHY_AUDIT", v)
		if got, want := DefaultPath(), filepath.Join(home, "audit", "audit.jsonl"); got != want {
			t.Errorf("BASHY_AUDIT=%q: %q, want %q", v, got, want)
		}
	}
	t.Setenv("BASHY_AUDIT", "/x/y.jsonl")
	if got := DefaultPath(); got != "/x/y.jsonl" {
		t.Fatalf("explicit path = %q", got)
	}
}

func TestAppendFillsAndChains(t *testing.T) {
	p := filepath.Join(t.TempDir(), "a", "audit.jsonl")
	t.Setenv("BASHY_AUDIT", p)
	for range 2 {
		r, err := Append(Record{Action: "claim.force", Argv: []string{"claim"}})
		if err != nil {
			t.Fatal(err)
		}
		if r.Time == "" || r.Actor.PID == 0 {
			t.Fatalf("not filled: %+v", r)
		}
	}
	f, _ := os.Open(p)
	defer f.Close()
	if res := Verify(f); !res.OK || res.Records != 2 {
		t.Fatalf("chain = %+v", res)
	}
}
