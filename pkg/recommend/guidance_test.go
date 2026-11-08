package recommend

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEarliest(t *testing.T) {
	if k, ok := Earliest(KindShape, KindLearn, KindRegister); !ok || k != KindRegister {
		t.Fatalf("got %v %v", k, ok)
	}
	if k, _ := Earliest(KindShape, KindReuse); k != KindReuse {
		t.Fatalf("got %v", k)
	}
	if _, ok := Earliest(KindCorrect); ok {
		t.Fatal("correct is outside the order")
	}
	if _, ok := Earliest(); ok {
		t.Fatal("empty")
	}
	if Rank(KindReuse) != 0 || Rank(KindShape) != 3 || Rank(KindCorrect) != -1 {
		t.Fatal("rank")
	}
}

func TestGuidanceJSON(t *testing.T) {
	g := NewGuidance(KindReuse, "jq exists")
	g.Command = []string{"jq", "."}
	g.Executable = true
	b, err := MarshalGuidance(g)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"schema_version", "kind", "reason", "evidence", "command", "prerequisites", "verify", "authorization", "executable"} {
		if _, ok := m[k]; !ok {
			t.Errorf("missing %s in %s", k, b)
		}
	}
	if m["schema_version"] != "bashy-recommend-v1" || m["authorization"] != "none" {
		t.Fatalf("%s", b)
	}
}

func TestGuidanceLine(t *testing.T) {
	g := NewGuidance(KindRegister, "add adapter")
	g.Command = []string{"bashy", "adapt"}
	g.Authorization = AuthApproval
	g.Verify = "bashy adapt --help"
	l := g.Line()
	if strings.Count(l, "\n") != 1 || !strings.Contains(l, "propose: bashy adapt") ||
		!strings.Contains(l, "needs approval") || !strings.Contains(l, "verify:") {
		t.Fatalf("%q", l)
	}
}
