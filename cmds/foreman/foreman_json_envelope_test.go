package foremancmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/foreman"
)

var errTestBoom = errors.New("boom")

func jsonTestRC(dir string) (*tool.RunContext, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: dir, Stdio: tool.Stdio{Out: &out, Err: &errb}}
	return rc, &out, &errb
}

// An empty `foreman list --json` must be an object envelope, never bare null:
// JSON consumers cannot distinguish null from "no sessions" without it.
func TestListJSONEmptyEnvelope(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	rc, out, errb := jsonTestRC(t.TempDir())
	if code := run(rc, []string{"list", "--json"}); code != 0 {
		t.Fatalf("list --json: code %d, err %s", code, errb.String())
	}
	var env struct {
		SchemaVersion string            `json:"schema_version"`
		Sessions      []json.RawMessage `json:"sessions"`
	}
	dec := json.NewDecoder(strings.NewReader(out.String()))
	if err := dec.Decode(&env); err != nil {
		t.Fatalf("list --json must emit one JSON object, got %q: %v", out.String(), err)
	}
	if env.SchemaVersion != foremanSchemaVersion {
		t.Fatalf("schema_version = %q, want %q", env.SchemaVersion, foremanSchemaVersion)
	}
	if env.Sessions == nil || len(env.Sessions) != 0 {
		t.Fatalf("sessions must normalize to [], got %q", out.String())
	}
}

// ok/fail map payloads carry the envelope version additively.
func TestOkJSONCarriesSchemaVersion(t *testing.T) {
	rc, out, _ := jsonTestRC(t.TempDir())
	if code := ok(rc, true, map[string]any{"id": "x"}); code != 0 {
		t.Fatalf("ok: code %d", code)
	}
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("ok must emit JSON, got %q: %v", out.String(), err)
	}
	if m["schema_version"] != foremanSchemaVersion {
		t.Fatalf("ok payload = %q, want schema_version %q", out.String(), foremanSchemaVersion)
	}
	if m["ok"] != true || m["id"] != "x" {
		t.Fatalf("ok payload lost its fields: %q", out.String())
	}
}

func TestFailJSONCarriesSchemaVersion(t *testing.T) {
	rc, out, _ := jsonTestRC(t.TempDir())
	// In --json mode the failure rides in the payload ({"ok":false,...});
	// fail returns emitJSON's code, unchanged by this story.
	fail(rc, true, errTestBoom)
	var m map[string]any
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("fail must emit JSON, got %q: %v", out.String(), err)
	}
	if m["schema_version"] != foremanSchemaVersion {
		t.Fatalf("fail payload = %q, want schema_version %q", out.String(), foremanSchemaVersion)
	}
	if m["ok"] != false {
		t.Fatalf("fail payload lost ok:false: %q", out.String())
	}
}

// The conductor skill reads `foreman status --json | jq '{steering, steer_why_not}'`:
// the status snapshot stays a bare State object with those fields addressable.
func TestStatusJSONKeepsStateShape(t *testing.T) {
	t.Setenv("BASHY_FOREMAN_DIR", t.TempDir())
	s, err := foreman.Start(context.Background(), foreman.Options{ID: "st-shape", Goal: "shapely", Agent: "stub", Runner: &stubRunner{out: "ack"}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rc, out, errb := jsonTestRC(t.TempDir())
	if code := run(rc, []string{"status", "--json", "st-shape"}); code != 0 {
		t.Fatalf("status --json: code %d, err %s", code, errb.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(out.Bytes(), &raw); err != nil {
		t.Fatalf("status --json must emit one JSON object, got %q: %v", out.String(), err)
	}
	if _, ok := raw["steering"]; !ok {
		t.Fatalf("status --json lost the steering field: %q", out.String())
	}
	var st foreman.State
	if err := json.Unmarshal(out.Bytes(), &st); err != nil {
		t.Fatalf("status --json must still decode as foreman.State: %v", err)
	}
}
