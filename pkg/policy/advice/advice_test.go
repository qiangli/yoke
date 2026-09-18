// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package advice

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const validFile = `{
  "schema": "bashy-advice-v1",
  "rules": [
    {"id": "trace-deploy", "name": "deploy_*", "decorator": "trace"},
    {"name": "*", "file": "src/*.sh", "decorator": "guard", "args": {"effects": "read,net"}},
    {"agentic": true, "decorator": "trace"},
    {"name": "boot_*", "exclude": [], "decorator": "trace"}
  ]
}`

func parseValid(t *testing.T) *Rules {
	t.Helper()
	r, err := Parse([]byte(validFile))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestParseValid(t *testing.T) {
	r := parseValid(t)
	if r.Len() != 4 {
		t.Fatalf("Len = %d, want 4", r.Len())
	}
	all := r.All()
	if all[0].ID != "trace-deploy" {
		t.Fatalf("rule 1 id = %q, want the explicit id", all[0].ID)
	}
	if !strings.HasPrefix(all[1].ID, "r-") || len(all[1].ID) != 14 {
		t.Fatalf("rule 2 id = %q, want a derived r-<12 hex> id", all[1].ID)
	}
	if got := all[1].Spec.String(); got != `@guard(effects: "read,net")` {
		t.Fatalf("rule 2 spec = %s", got)
	}
	if !all[0].ExcludePreamble || all[3].ExcludePreamble {
		t.Fatalf("exclude defaults: rule 1 = %t (want true), rule 4 = %t (want false)",
			all[0].ExcludePreamble, all[3].ExcludePreamble)
	}
}

// Every load-time rejection the contract names, each failing loudly with the
// offending rule identified.
func TestParseErrors(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{"bad json", `{`, "parse rules"},
		{"wrong schema", `{"schema": "bashy-advice-v2", "rules": []}`, `schema "bashy-advice-v2"`},
		{"missing schema", `{"rules": []}`, "schema"},
		{"unknown field", `{"schema": "bashy-advice-v1", "rules": [{"nmae": "x", "decorator": "trace"}]}`, "unknown field"},
		{"trailing data", `{"schema": "bashy-advice-v1", "rules": []} {}`, "trailing data"},
		{"trailing closing brace", `{"schema": "bashy-advice-v1", "rules": []} }`, "trailing data"},
		{"trailing closing bracket", `{"schema": "bashy-advice-v1", "rules": []} ]`, "trailing data"},
		{"trailing garbage", `{"schema": "bashy-advice-v1", "rules": []} garbage`, "trailing data"},
		{"no selector", `{"schema": "bashy-advice-v1", "rules": [{"decorator": "trace"}]}`, "no selector"},
		{"no decorator", `{"schema": "bashy-advice-v1", "rules": [{"name": "*"}]}`, "no decorator"},
		{"bad name glob", `{"schema": "bashy-advice-v1", "rules": [{"name": "[", "decorator": "trace"}]}`, `name glob "["`},
		{"bad file glob", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "file": "[", "decorator": "trace"}]}`, `file glob "["`},
		{"bad exclude", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "exclude": ["callsite"], "decorator": "trace"}]}`, `unknown exclude "callsite"`},
		{"unknown decorator", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "audit"}]}`, `unknown decorator "audit"`},
		{"log not implemented", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "log"}]}`, "not implemented yet"},
		{"retry never", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "retry", "args": {"n": 3}}]}`, "never advisable"},
		{"memo never", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "memo"}]}`, "never advisable"},
		{"timeout never", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "timeout"}]}`, "never advisable"},
		{"trace with args", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "trace", "args": {"x": 1}}]}`, "trace takes no arguments"},
		{"guard no args", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "guard"}]}`, "guard takes exactly one argument"},
		{"guard wrong arg", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "guard", "args": {"cap": "read"}}]}`, "guard takes exactly one argument"},
		{"guard non-string", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "guard", "args": {"effects": 3}}]}`, "guard effects must be a string"},
		{"guard bad effect", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "guard", "args": {"effects": "read,madeup"}}]}`, `unknown effect "madeup"`},
		{"guard empty cap", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "guard", "args": {"effects": " , "}}]}`, "empty effect cap"},
		{"float arg", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "trace", "args": {"x": 1.5}}]}`, "not an integer"},
		{"array arg", `{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "trace", "args": {"x": []}}]}`, "unsupported value type"},
		{"duplicate explicit id", `{"schema": "bashy-advice-v1", "rules": [
			{"id": "a", "name": "x", "decorator": "trace"},
			{"id": "a", "name": "y", "decorator": "trace"}]}`, `duplicate id "a"`},
		{"duplicate identical rule", `{"schema": "bashy-advice-v1", "rules": [
			{"name": "x", "decorator": "trace"},
			{"name": "x", "decorator": "trace"}]}`, "duplicate id"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Parse([]byte(c.body))
			if err == nil {
				t.Fatalf("Parse accepted %s", c.name)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error = %q, want it to contain %q", err, c.want)
			}
			if !strings.HasPrefix(err.Error(), "advice: ") {
				t.Fatalf("error %q does not carry the advice: prefix", err)
			}
		})
	}
}

func TestSelectors(t *testing.T) {
	r := parseValid(t)
	cases := []struct {
		name string
		q    Query
		want []string // matching decorators in order, as rule identifiers
	}{
		{"name glob", Query{Name: "deploy_prod"}, []string{"trace-deploy"}},
		{"name glob miss", Query{Name: "deployX"}, nil},
		{"file glob", Query{Name: "f", File: "src/lib.sh"}, []string{"guard"}},
		{"file glob no ** crossing", Query{Name: "f", File: "src/sub/lib.sh"}, nil},
		{"file glob windows path", Query{Name: "f", File: `src\lib.sh`}, []string{"guard"}},
		{"agentic", Query{Name: "sum", Agentic: true}, []string{"agentic-trace"}},
		{"agentic false does not match", Query{Name: "sum"}, nil},
		{"and semantics", Query{Name: "deploy_a", File: "src/a.sh", Agentic: true},
			[]string{"trace-deploy", "guard", "agentic-trace"}},
	}
	all := r.All()
	label := map[string]string{
		all[0].ID: "trace-deploy", all[1].ID: "guard", all[2].ID: "agentic-trace", all[3].ID: "boot",
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, a := range r.For(c.q) {
				got = append(got, label[a.RuleID])
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("For(%+v) = %v, want %v", c.q, got, c.want)
			}
		})
	}
}

// Preamble functions are out of scope by default; only an explicit
// "exclude": [] reaches them.
func TestPreambleExcludedByDefault(t *testing.T) {
	r := parseValid(t)
	if got := r.For(Query{Name: "deploy_x", Preamble: true}); got != nil {
		t.Fatalf("preamble matched the default-exclude rule: %+v", got)
	}
	got := r.For(Query{Name: "boot_net", Preamble: true})
	if len(got) != 1 || got[0].Spec.Decorator != "trace" {
		t.Fatalf("opt-in rule did not reach preamble: %+v", got)
	}
}

// Matching is pure and ordered: repeated calls agree, results come in file
// order, and mutating a returned slice cannot leak into the rule set.
func TestDeterministicOrderAndIdempotence(t *testing.T) {
	r := parseValid(t)
	q := Query{Name: "deploy_a", File: "src/a.sh", Agentic: true}
	first := r.For(q)
	if len(first) != 3 {
		t.Fatalf("For = %+v, want 3 advised", first)
	}
	first[0].Spec.Decorator = "clobbered"
	first[1].Spec.Args[0].Value.Str = "clobbered"
	second := r.For(q)
	third := r.For(q)
	if !reflect.DeepEqual(second, third) {
		t.Fatalf("For is not deterministic: %+v vs %+v", second, third)
	}
	if second[0].Spec.Decorator != "trace" || second[1].Spec.Args[0].Value.Str != "read,net" {
		t.Fatalf("mutating a For result leaked into the rule set: %+v", second)
	}

	all := r.All()
	*all[2].Agentic = false
	if len(r.For(q)) != 3 {
		t.Fatalf("mutating Agentic bool in All result leaked into the rule set")
	}
	// Derived IDs are content-stable: a reload and a reorder both keep them.
	again := parseValid(t)
	if !reflect.DeepEqual(r.For(q), again.For(q)) {
		t.Fatal("reload changed the advised set")
	}
	reordered := `{"schema": "bashy-advice-v1", "rules": [
		{"name": "*", "file": "src/*.sh", "decorator": "guard", "args": {"effects": "read,net"}},
		{"id": "trace-deploy", "name": "deploy_*", "decorator": "trace"}]}`
	r2, err := Parse([]byte(reordered))
	if err != nil {
		t.Fatal(err)
	}
	if r2.All()[0].ID != r.All()[1].ID {
		t.Fatalf("derived id depends on position: %q vs %q", r2.All()[0].ID, r.All()[1].ID)
	}
}

func TestFromEnv(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "advice.json")
	if err := os.WriteFile(p, []byte(validFile), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("off by default", func(t *testing.T) {
		for _, env := range [][]string{nil, {"BASHY_ADVICE="}} {
			r, err := FromEnv(env)
			if r != nil || err != nil {
				t.Fatalf("FromEnv(%v) = %v, %v; want nil, nil", env, r, err)
			}
			if got := r.For(Query{Name: "deploy_x"}); got != nil {
				t.Fatalf("nil Rules advised %+v", got)
			}
		}
	})
	t.Run("opt in", func(t *testing.T) {
		r, err := FromEnv([]string{"BASHY_ADVICE=" + p})
		if err != nil || r.Len() != 4 {
			t.Fatalf("FromEnv = %v, %v", r, err)
		}
	})
	t.Run("cert is zero rules", func(t *testing.T) {
		r, err := FromEnv([]string{"BASHY_ADVICE=" + p, "VSC_PROFILE=cert"})
		if r != nil || err != nil {
			t.Fatalf("cert FromEnv = %v, %v; want nil, nil", r, err)
		}
		// Cert never opens the file: a bad path must not surface an error.
		r, err = FromEnv([]string{"BASHY_ADVICE=" + filepath.Join(dir, "missing.json"), "VSC_PROFILE=cert"})
		if r != nil || err != nil {
			t.Fatalf("cert with bad path = %v, %v; want nil, nil", r, err)
		}
	})
	t.Run("missing file errors", func(t *testing.T) {
		_, err := FromEnv([]string{"BASHY_ADVICE=" + filepath.Join(dir, "missing.json")})
		if err == nil || !strings.Contains(err.Error(), "missing.json") {
			t.Fatalf("err = %v, want the path named", err)
		}
	})
	t.Run("load error names the file", func(t *testing.T) {
		bad := filepath.Join(dir, "bad.json")
		if err := os.WriteFile(bad, []byte(`{"schema": "bashy-advice-v1", "rules": [{"name": "*", "decorator": "retry"}]}`), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := FromEnv([]string{"BASHY_ADVICE=" + bad})
		if err == nil || !strings.Contains(err.Error(), "never advisable") || !strings.Contains(err.Error(), "bad.json") {
			t.Fatalf("err = %v, want the rejection and the file named", err)
		}
	})
}
