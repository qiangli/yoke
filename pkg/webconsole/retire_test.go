package webconsole

import (
	"bytes"
	"github.com/qiangli/yoke/pkg/fleet"
	"strings"
	"testing"
)

func TestRetiredAppDiscoveryAndList(t *testing.T) {
	writeAppRecord(t, "example", "name: example\nport: 8888\nretired:\n  at: yesterday\n  replaced_by: next\n")
	for _, p := range Discover() {
		if p.Name == "example" {
			t.Fatal("retired app discovered")
		}
	}
	if _, ok := fleet.New().App("example"); !ok {
		t.Fatal("retired app no longer resolves")
	}
	cmd := NewAppsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"list", "--retired", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"example"`) || !strings.Contains(out.String(), `"retired"`) {
		t.Fatalf("retired list: %s", out.String())
	}
}
