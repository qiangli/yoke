package bscript

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/redact"
	"github.com/qiangli/yoke/pkg/skills"
)

func TestLowerCommandDeterministicAndRoundTrips(t *testing.T) {
	req := Request{
		Kind:     Command,
		Args:     []string{"missing-tool", "two words", "a'b"},
		Scrubber: redact.New(),
	}
	a, err := Lower(req)
	if err != nil {
		t.Fatal(err)
	}
	b, err := Lower(req)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("same request produced different yields:\n%#v\n%#v", a, b)
	}
	if a.Status != StatusInputRequired || a.Skill.Kind != skills.RecordKind {
		t.Fatalf("yield header = %#v", a)
	}
	if got := a.Skill.Bindings["step-run"]; got != `missing-tool 'two words' 'a'"'"'b'` {
		t.Fatalf("step-run = %q", got)
	}
	recordBytes, err := skills.MarshalRecord(a.Skill)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := skills.ParseRecord(recordBytes)
	if err != nil {
		t.Fatal(err)
	}
	sk, err := skills.ParseFrontmatter([]byte(parsed.Files["SKILL.md"]))
	if err != nil {
		t.Fatal(err)
	}
	if sk.Meta["step-run"] != a.Skill.Bindings["step-run"] {
		t.Fatalf("frontmatter step-run = %q, want %q", sk.Meta["step-run"], a.Skill.Bindings["step-run"])
	}
	if parsed.Identity != "" || parsed.Capability != "" || parsed.Contract.Steps != 0 {
		t.Fatalf("yield invented a contract: %#v", parsed)
	}
}

func TestLowerSymbolizesHostIdentity(t *testing.T) {
	const host = "private-workstation.example"
	y, err := Lower(Request{
		Kind:     Command,
		Args:     []string{"ssh", host},
		Scrubber: redact.New(redact.WithHost(host)),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := skills.MarshalRecord(y.Skill)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), host) {
		t.Fatalf("record contains host identity: %s", b)
	}
	if !strings.Contains(string(b), "‹host:") {
		t.Fatalf("record does not contain symbolic host: %s", b)
	}
}

func TestLowerScriptAndInvalidRequests(t *testing.T) {
	y, err := Lower(Request{Kind: Script, Source: "printf '%s\\n' ok", Scrubber: redact.New()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(y.Skill.Bindings["step-run"], "bashy -c ") {
		t.Fatalf("script binding = %q", y.Skill.Bindings["step-run"])
	}
	y, err = Lower(Request{Kind: Script, Args: []string{"./broken.sh", "two words"}, Scrubber: redact.New()})
	if err != nil {
		t.Fatal(err)
	}
	if got := y.Skill.Bindings["step-run"]; got != "bashy ./broken.sh 'two words'" {
		t.Fatalf("script path binding = %q", got)
	}
	for _, req := range []Request{{Kind: Command}, {Kind: Script}, {Kind: "text"}} {
		if _, err := Lower(req); err == nil {
			t.Fatalf("Lower(%#v) succeeded", req)
		}
	}
}
