package skills

import "github.com/qiangli/yoke/pkg/fleet"

// Agent detection: which agentic tool is driving this process, from the env
// markers each agent sets (the CI=true analog of the agent world). Detection
// is free and write-less — the bottom rung of the advertisement ladder.
//
// The marker table moved into the fleet registry (coreutils/pkg/fleet), where
// it sits beside everything else known about a tool: its binary, its launch
// contract, its harness scores. Recognizing a new harness is now `bashy tool
// add`, not an edit here.

// DetectAgent reports the agentic tool driving this process, if any.
//
// The name is a TOOL (claude, codex), not an agent: an agent names a tool AND
// a model, and this process's environment says nothing about the model.
func DetectAgent() (name string, ok bool) { return fleet.DetectTool() }
