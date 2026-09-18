package secrets

import "testing"

func TestParseEnvRoundTrips(t *testing.T) {
	// Values chosen to exercise the single-quote escaping (o'brien) and skipped
	// lines (comment, blank, malformed).
	bindings := []binding{
		{local: "GITHUB_TOKEN", literal: "ghp_xyz", isRef: false},
		{local: "TRICKY", literal: "a'b'c", isRef: false},
		{local: "PLAIN", literal: "hello world", isRef: false},
	}
	rendered, _ := renderEnv(bindings, nil)
	got := ParseEnv(rendered)

	want := map[string]string{"GITHUB_TOKEN": "ghp_xyz", "TRICKY": "a'b'c", "PLAIN": "hello world"}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("ParseEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("ParseEnv returned %d entries, want %d: %v", len(got), len(want), got)
	}
}

func TestParseEnvSkipsCommentsAndInvalid(t *testing.T) {
	in := []byte("# bashy secret: served from cache\n" +
		"export GITHUB_TOKEN='ghp_ok'\n" +
		"\n" +
		"export 1BAD='nope'\n" + // invalid env name -> skipped
		"garbage line without equals\n")
	got := ParseEnv(in)
	if got["GITHUB_TOKEN"] != "ghp_ok" {
		t.Fatalf("GITHUB_TOKEN = %q, want ghp_ok", got["GITHUB_TOKEN"])
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 entry, got %d: %v", len(got), got)
	}
}
