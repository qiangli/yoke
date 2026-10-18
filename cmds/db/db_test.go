package dbcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func TestRegisteredLifecycleCommand(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "one.db")
	registered := tool.Lookup("db")
	if registered == nil {
		t.Fatal("db tool not registered")
	}
	run := func(args ...string) (int, map[string]any) {
		t.Helper()
		var out, stderr bytes.Buffer
		rc := &tool.RunContext{Ctx: context.Background(), Dir: root, Env: os.Environ()}
		rc.Out = &out
		rc.Err = &stderr
		code := registered.Run(rc, args)
		var doc map[string]any
		if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
			t.Fatalf("%v: output %q stderr %q: %v", args, out.String(), stderr.String(), err)
		}
		return code, doc
	}
	flags := []string{"--engine", "sqlite", "--mode", "persistent", "--path", path}
	if code, doc := run(append([]string{"show"}, flags...)...); code != 1 || doc["schema_version"] != "bashy-db-error-v1" {
		t.Fatalf("missing show: code=%d doc=%v", code, doc)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("show created path: %v", err)
	}
	if code, doc := run(append([]string{"create"}, flags...)...); code != 0 || doc["schema_version"] != "bashy-db-result-v1" {
		t.Fatalf("create: code=%d doc=%v", code, doc)
	}
	if code, doc := run("session", "start", "--dry-run"); code != 0 || doc["preview_level"] != "local-preview" {
		t.Fatalf("session dry-run: code=%d doc=%v", code, doc)
	}
	if code, doc := run(append([]string{"check"}, flags...)...); code != 0 || doc["ok"] != true {
		t.Fatalf("check: code=%d doc=%v", code, doc)
	}
	if code, _ := run(append([]string{"drop"}, flags...)...); code != 2 {
		t.Fatalf("unconfirmed drop code=%d", code)
	}
	if code, doc := run(append(append([]string{"drop"}, flags...), "--yes")...); code != 0 || doc["ok"] != true {
		t.Fatalf("drop: code=%d doc=%v", code, doc)
	}
}
