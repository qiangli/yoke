// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// A weave queue item is a RUN: ONE agent working ONE issue, in one isolated workspace,
// on one branch, behind one gate.
//
// # Why the word changed
//
// weave used to call its queue items "issues". That collided head-on with `bashy
// issue` — the durable, committed register of what is wrong and what is wanted — and
// two senses of one word in one system is how a lexicon dies.
//
// The replacement is not a preference; it is the word weave ALREADY used. Its own
// reporter converts a queue item into an UpsertRunReq, which the control plane stores
// as a SprintRun and defines as "one agent × one issue". The code has been saying "run"
// on the wire the whole time while saying "issue" at the prompt.
//
// It is also NOT "task", tempting as that sounds: the published hierarchy is
//
//	issue  ->  run  ->  sprint  ->  task
//	what     one agent   one bounded   a durable goal driving
//	is       working     orchestrated  0..N sprints across
//	wanted   one issue   pass          hosts, agents and time
//
// so "task" already names the thing ABOVE sprint. Had a queue item become a "task",
// bashy's "task" would have meant the control plane's "run", and nothing would have
// been left to name the actual task. That is an inversion, and it is worse than the
// collision it would have fixed.
//
// # Compatibility
//
// Day 1 (now): --run is canonical, --issue still works; every JSON envelope carries
// BOTH the "run" and "issue" keys, so no existing consumer, script or agent breaks.
// Day 2: --issue and the "issue" key are dropped. This is the umbrella's standing
// protocol-evolution rule, and a rename is exactly what it exists for.

// runFlag registers the canonical --run and keeps --issue working as a silent alias.
//
// Both write the SAME variable, so a caller may pass either and the code below never
// has to know which was used.
func runFlag(cmd *cobra.Command, p *int64, usage string) {
	cmd.Flags().Int64Var(p, "run", 0, usage)
	cmd.Flags().Int64Var(p, "issue", 0, usage+" (deprecated alias for --run)")
	_ = cmd.Flags().MarkDeprecated("issue", "use --run: a weave queue item is a run (one agent × one issue); `bashy issue` is now the issue register")
}

// emitOK is weavecli.EmitOK plus the Day-1 compatibility alias.
//
// Every envelope that carries a "run" also carries an "issue" with the same value (and
// vice versa), so a script or agent written against either vocabulary keeps working
// through the transition. Dropping the alias is a Day-2 change, made in one place.
func emitOK(w io.Writer, mode weavecli.OutputMode, name string, result any) int {
	if m, ok := result.(map[string]any); ok {
		if v, has := m["run"]; has {
			if _, dup := m["issue"]; !dup {
				m["issue"] = v
			}
		} else if v, has := m["issue"]; has {
			m["run"] = v
		}
	}
	return weavecli.EmitOK(w, mode, name, result)
}
