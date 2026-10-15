package fleet

import (
	"github.com/qiangli/yoke/pkg/snippet"
	"testing"
)

func TestUsageProbeYAMLValidation(t *testing.T) {
	good := []byte("name: test\nusage:\n  - id: quota\n    run: 'echo {}'\n    metrics:\n      - name: used_percent\n        value: .usage.pct\n        unit: percent\n        window: 5h\n        reset: .usage.reset\n")
	m, err := ParseModel("test", good, nil)
	if err != nil || len(m.Usage) != 1 || m.Usage[0].RefreshDuration().Minutes() != 5 {
		t.Fatalf("model usage: %+v %v", m.Usage, err)
	}
	if _, err = ParsePlan("test", good, nil); err != nil {
		t.Fatal(err)
	}
	if d := (UsageProbe{Run: snippet.Snippet{Body: "agentic { echo quota; }"}}).RefreshDuration(); d.Hours() != 1 {
		t.Fatalf("agentic default refresh %v", d)
	}
	for _, tc := range []struct{ name, body string }{
		{"bad vocabulary", "name: x\nusage:\n- id: q\n  run: 'echo {}'\n  metrics:\n  - name: made_up\n    value: .x\n"},
		{"bad jq", "name: x\nusage:\n- id: q\n  run: 'echo {}'\n  metrics:\n  - name: used\n    value: .[\n"},
		{"duplicate", "name: x\nusage:\n- id: q\n  run: 'echo {}'\n- id: q\n  run: 'echo {}'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseModel("x", []byte(tc.body), nil); err == nil {
				t.Fatal("invalid usage accepted")
			}
		})
	}
}
