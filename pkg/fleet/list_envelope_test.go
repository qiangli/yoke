package fleet

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// decodeListItems unwraps a kind's `list --json` envelope, failing the test on
// a bare array or a wrong schema.
func decodeListItems[T any](t *testing.T, data []byte) []T {
	t.Helper()
	var env struct {
		SchemaVersion string `json:"schema_version"`
		Kind          string `json:"kind"`
		View          string `json:"view"`
		Items         []T    `json:"items"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("list --json is not an envelope: %v\n%s", err, data)
	}
	if env.SchemaVersion != ListSchemaVersion {
		t.Fatalf("schema_version = %q, want %q\n%s", env.SchemaVersion, ListSchemaVersion, data)
	}
	return env.Items
}

func TestWriteListJSONShape(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteListJSON[string](&buf, "tool", "custom", nil); err != nil {
		t.Fatal(err)
	}
	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["schema_version"] != "bashy-registry-list-v1" || env["kind"] != "tool" || env["view"] != "custom" {
		t.Errorf("envelope = %v", env)
	}
	if items, ok := env["items"].([]any); !ok || len(items) != 0 {
		t.Errorf("nil items must encode as [], got %v", env["items"])
	}
}

func TestBasicView(t *testing.T) {
	for _, c := range []struct {
		retired, all bool
		want         string
	}{{false, false, "default"}, {true, false, "retired"}, {false, true, "all"}, {true, true, "retired"}} {
		if got := BasicView(c.retired, c.all); got != c.want {
			t.Errorf("BasicView(%v,%v) = %q, want %q", c.retired, c.all, got, c.want)
		}
	}
}

// Every kind's list --json carries the same envelope, and the view names the
// flag that selected it.
func TestListEnvelopeEveryKind(t *testing.T) {
	roots := map[string]func(...Option) *cobra.Command{KindTool: NewToolsCmd, KindModel: NewModelsCmd, KindAgent: NewAgentsCmd}
	for kind, root := range roots {
		t.Run(kind, func(t *testing.T) {
			_, opts := viewFixture(t, kind)
			for flag, view := range map[string]string{"": "default", "--custom": "custom", "--builtin": "builtin", "--active": "active", "--retired": "retired", "--all": "all"} {
				args := []string{"list", "--json"}
				if flag != "" {
					args = append(args, flag)
				}
				out, err := runCmd(t, root(opts...), args...)
				if err != nil {
					t.Fatalf("%v: %v", args, err)
				}
				var raw map[string]json.RawMessage
				if err := json.Unmarshal([]byte(out), &raw); err != nil {
					t.Fatalf("%v: not an object: %v\n%s", args, err, out)
				}
				for k, want := range map[string]string{"schema_version": ListSchemaVersion, "kind": kind, "view": view} {
					if got := strings.Trim(string(raw[k]), `"`); got != want {
						t.Errorf("%v: %s = %q, want %q", args, k, got, want)
					}
				}
				if string(raw["items"]) == "null" || raw["items"] == nil {
					t.Errorf("%v: items missing or null", args)
				}
			}
		})
	}
}
