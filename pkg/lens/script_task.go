package lens

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ScriptTask is the body contract shared by sprint assignment and lens-script.
// Artifact.Path is informational; only the embedded, verified bytes execute.
type ScriptTask struct {
	Schema   string                 `json:"schema"`
	Artifact CompiledScriptArtifact `json:"artifact"`
	Caps     []string               `json:"caps"`
	Effects  []string               `json:"effects"`
	Help     string                 `json:"help"`
	NoInput  bool                   `json:"no_input"`
}

// DecodeScriptTask validates a bounded task without executing it or reading its artifact path.
func DecodeScriptTask(body []byte) (*ScriptTask, error) {
	if len(body) == 0 || len(body) > 64*1024 {
		return nil, fmt.Errorf("task body must be 1..65536 bytes")
	}
	var task ScriptTask
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&task); err != nil {
		return nil, err
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("trailing task data")
	}
	if task.Schema != "lens-script-task-v1" || task.Help != "script" || !task.NoInput {
		return nil, fmt.Errorf("requires lens-script-task-v1, help:script and no_input:true")
	}
	for _, values := range [][]string{task.Caps, task.Effects} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" || strings.Contains(value, ",") {
				return nil, fmt.Errorf("invalid policy entry")
			}
		}
	}
	runner := LocalScriptRunner{AllowedCaps: task.Caps, AllowedEffects: task.Effects}
	if err := runner.ValidateScript(&task.Artifact); err != nil {
		return nil, err
	}
	return &task, nil
}

// WorkerArgs repeats the validated identity and policy at the receiving boundary.
func (t *ScriptTask) WorkerArgs() []string {
	return []string{"lens-script", "--digest", t.Artifact.Digest, "--caps", strings.Join(t.Caps, ","), "--effects", strings.Join(t.Effects, ",")}
}
