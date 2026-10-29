// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package unit_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/qiangli/yoke/pkg/unit"
)

func TestReadContractBothForms(t *testing.T) {
	body := `cat <<'DOC'
@ensure("not a contract: inside a heredoc")
DOC
@require("test -d .")
@ensure(check: "test -f out")
@effects("read, write")
@guard(effects: "read,write,net")
@confirm()
@timeout("30s")
work() { touch out; }
work
`
	c, err := unit.ReadContract(body)
	if err != nil {
		t.Fatal(err)
	}
	want := unit.Contract{Require: []string{"test -d ."}, Ensure: []string{"test -f out"},
		Effects: []string{"read", "write"}, Guard: []string{"read", "write", "net"},
		Confirm: []string{"on"}, Timeout: "30s"}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("contract = %+v\nwant       %+v", c, want)
	}
	positional, _ := unit.ReadContract("@require(\"test -d .\")\n@ensure(\"test -f out\")\n@guard(\"write,read\")\nwork() { :; }\n")
	k1, err1 := unit.CapabilityKey(c)
	k2, err2 := unit.CapabilityKey(unit.Contract{Require: positional.Require, Ensure: positional.Ensure, Effects: []string{"write", "read"}})
	if err1 != nil || err2 != nil || k1 != k2 {
		t.Fatalf("one promise, two spellings, two keys: %q %v / %q %v", k1, err1, k2, err2)
	}
	if _, err := unit.CapabilityKey(unit.Contract{Effects: []string{"read"}}); !errors.Is(err, unit.ErrNoContract) {
		t.Fatalf("effects-only contract keyed: %v", err)
	}
}

func TestBashSharpBodyNestedAndUnterminated(t *testing.T) {
	doc := "```text\n```bsh\nignored\n```\n```bsh\nfirst\n```\nprose\n~~~bashsharp\nsecond\n~~~\n"
	if got := unit.BashSharpBody(doc); got != "first\nsecond\n" {
		t.Fatalf("body = %q", got)
	}
	if got := unit.BashSharpBody("```bsh\nopen\n"); got != "" {
		t.Fatalf("unterminated fence yielded %q", got)
	}
}
