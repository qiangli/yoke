package lens

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScriptTaskAdmission(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		ok           bool
	}{
		{"literal", "echo bounded\n", true},
		{"ci-build", "go build ./...\n", false},
		{"ci-test", "go test ./...\n", false},
		{"artifact", "printf release > artifact.txt\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := []byte(tc.script)
			task := ScriptTask{Schema: "lens-script-task-v1", Artifact: CompiledScriptArtifact{Name: ScriptArtifactName(b), Digest: ScriptArtifactDigest(b), Script: b}, Caps: []string{"read-only"}, Effects: []string{"pure"}, Help: "script", NoInput: true}
			body, _ := json.Marshal(task)
			got, err := DecodeScriptTask(body)
			if (err == nil) != tc.ok {
				t.Fatalf("admission=%v want success=%v", err, tc.ok)
			}
			if err != nil {
				t.Logf("blocked: %v", err)
				return
			}
			if string(got.Artifact.Script) != tc.script {
				t.Fatal("bytes changed")
			}
			for _, change := range []func(*ScriptTask){
				func(t *ScriptTask) { t.Caps = nil }, func(t *ScriptTask) { t.Effects = nil },
				func(t *ScriptTask) { t.Caps = []string{"read-only,write"} }, func(t *ScriptTask) { t.Artifact.Script = []byte("true\n") },
				func(t *ScriptTask) { t.Artifact.Name = "bad.sh" }, func(t *ScriptTask) { t.Help = "agent" },
			} {
				copy := task
				change(&copy)
				bad, _ := json.Marshal(copy)
				if _, err := DecodeScriptTask(bad); err == nil {
					t.Fatalf("accepted %s", bad)
				}
			}
			for _, bad := range [][]byte{nil, []byte(strings.Repeat(" ", 65537)), append(body, body...), []byte(strings.Replace(string(body), `"schema":`, `"unknown":1,"schema":`, 1))} {
				if _, err := DecodeScriptTask(bad); err == nil {
					t.Fatal("invalid framing accepted")
				}
			}
		})
	}
}
