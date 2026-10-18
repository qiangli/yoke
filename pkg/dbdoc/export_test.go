package dbdoc

import (
	"encoding/json"
	"testing"

	"github.com/qiangli/yoke/pkg/dbstore"
)

// The exported wrappers are the document grammar itself, not a copy.
func TestExportedFilterAndObjectMatchDocGrammar(t *testing.T) {
	f, p := json.RawMessage(`{"and":[{"field":"o.v","op":"gt","param":"lo"},{"field":"s","op":"eq","value":"x"}]}`), json.RawMessage(`{"lo":15}`)
	w1, a1, err1 := compileFilter(f, p)
	w2, a2, err2 := CompileFilter(f, p)
	if err1 != nil || err2 != nil || w1 != w2 || len(a1) != len(a2) {
		t.Fatalf("CompileFilter diverges: %q %v %v / %q %v %v", w1, a1, err1, w2, a2, err2)
	}
	if _, _, err := CompileFilter(json.RawMessage(`{"field":"a'b","op":"eq","value":1}`), nil); dbstore.Code(err) != "invalid_input" {
		t.Fatalf("bad field: %v", err)
	}
	b, err := CanonicalObject("metadata", json.RawMessage(`{ "b":1.50, "a":[12345678901234567890] }`))
	if err != nil || string(b) != `{"a":[12345678901234567890],"b":1.50}` {
		t.Fatalf("canonical %s %v", b, err)
	}
	for _, bad := range []string{`[1]`, `{"a":1,"a":2}`, ``} {
		if _, err := CanonicalObject("metadata", json.RawMessage(bad)); dbstore.Code(err) != "invalid_input" {
			t.Fatalf("object %q: %v", bad, err)
		}
	}
}
