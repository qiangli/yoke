package dbcmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func kvEntries(t *testing.T, doc map[string]any) string {
	t.Helper()
	b, _ := json.Marshal(doc["result"].(map[string]any)["entries"])
	return string(b)
}

// TestKVVerbsPersistentCLI drives the registered `db kv` path end to end:
// explicit create, no implicit bucket, binary fidelity, CAS conflict code,
// TTL refusal, export, import into a second explicit target.
func TestKVVerbsPersistentCLI(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	a := []string{"--engine", "bbolt", "--mode", "persistent", "--path", "a.db"}
	b := []string{"--engine", "bbolt", "--mode", "persistent", "--path", "b.db"}
	run := func(want int, target []string, args ...string) map[string]any {
		t.Helper()
		code, doc := dbRun(t, root, "", append(append([]string{}, args...), target...)...)
		if code != want {
			t.Fatalf("db %v: exit %d (want %d): %v", args, code, want, doc)
		}
		return doc
	}
	run(0, a, "create")
	if doc := run(1, a, "kv", "put", "--bucket", "b", "--key", "text:k", "--value", "text:v"); doc["error"].(map[string]any)["code"] != "not_found" {
		t.Fatalf("implicit bucket creation: %v", doc)
	}
	run(0, a, "kv", "bucket", "create", "--bucket", "b")
	run(0, a, "kv", "put", "--bucket", "b", "--key", "b64:AP8=", "--value", "b64:/wA=")
	run(0, a, "kv", "put", "--bucket", "b", "--key", "text:k", "--value", "text:v")
	if doc := run(1, a, "kv", "cas", "--bucket", "b", "--key", "text:k", "--expect", "text:zz", "--value", "text:n"); doc["error"].(map[string]any)["code"] != "conflict" {
		t.Fatalf("cas: %v", doc)
	}
	run(2, a, "kv", "put", "--bucket", "b", "--key", "text:k", "--value", "text:v", "--ttl", "1m")
	run(2, a, "kv", "get", "--bucket", "b", "--key", "k")
	scan := kvEntries(t, run(0, a, "kv", "scan", "--bucket", "b"))
	if want := `[{"key":{"b64":"AP8="},"value":{"b64":"/wA="}},{"key":{"text":"k"},"value":{"text":"v"}}]`; scan != want {
		t.Fatalf("scan %s", scan)
	}
	exp := filepath.Join(root, "exp")
	run(0, a, "export", "--out", exp)
	if _, err := os.Stat(filepath.Join(exp, "manifest.json")); err != nil {
		t.Fatal(err)
	}
	run(0, b, "create")
	run(0, b, "import", "--in", exp)
	if got := kvEntries(t, run(0, b, "kv", "scan", "--bucket", "b")); got != scan {
		t.Fatalf("import %s", got)
	}
	// The same input document drives batch through --input.
	batch := `{"ops":[{"op":"put","bucket":"b","key":{"text":"z"},"value":{"text":"1"}},{"op":"check","bucket":"b","key":{"text":"k"},"expect":{"text":"nope"}}]}`
	code, doc := dbRun(t, root, batch, append([]string{"kv", "batch", "--input", "-"}, b...)...)
	if code != 1 || doc["error"].(map[string]any)["code"] != "conflict" {
		t.Fatalf("batch: %d %v", code, doc)
	}
	if got := kvEntries(t, run(0, b, "kv", "scan", "--bucket", "b", "--prefix", "text:z")); got != "null" && got != "[]" {
		t.Fatalf("rolled-back batch left %s", got)
	}
}

// TestIndependentKVSharedOwner shares one memory database between separate
// CLI calls through an explicit owner session.
func TestIndependentKVSharedOwner(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(root, "sessions"))
	t.Setenv("BASHY_COORD_DIR", filepath.Join(root, "coord"))
	code, doc := dbRun(t, root, "", "session", "start", "--idle-ttl", "2m")
	if code != 0 {
		t.Fatalf("session start: %v", doc)
	}
	session := doc["result"].(map[string]any)["id"].(string)
	defer dbRun(t, root, "", "session", "stop", "--session", session, "--yes")
	m := []string{"--engine", "bbolt", "--mode", "memory", "--session", session, "--name", "kv"}
	for _, step := range [][]string{
		{"create"},
		{"kv", "bucket", "create", "--bucket", "b"},
		{"kv", "put", "--bucket", "b", "--key", "text:k", "--value", "text:v", "--request-id", "r1"},
		{"kv", "put", "--bucket", "b", "--key", "text:k", "--value", "text:v", "--request-id", "r1"},
	} {
		if code, doc := dbRun(t, root, "", append(append([]string{}, step...), m...)...); code != 0 {
			t.Fatalf("%v: %v", step, doc)
		}
	}
	code, doc = dbRun(t, root, "", append([]string{"kv", "get", "--bucket", "b", "--key", "text:k"}, m...)...)
	if code != 0 || doc["result"].(map[string]any)["found"] != true {
		t.Fatalf("get via owner: %v", doc)
	}
}
