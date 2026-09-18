// Package resourcescmd implements `resources`: report fleet utilization across
// providers (Anthropic, OpenAI, Google, Zhipu, Moonshot, DeepSeek) and bands (L1-L4).
package resourcescmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/resources"
)

var cmd = &tool.Tool{
	Name:     "resources",
	Synopsis: "Report fleet utilization across providers, bands, and meters.",
	Usage:    "resources fleet [--json] | resources budget [MODEL...] [--json]",
}

const usageText = "Usage: resources fleet [--json]\n" +
	"       resources budget [MODEL...] [--json]\n"

func init() {
	cmd.Run = run
	tool.Register(cmd)
}

func run(rc *tool.RunContext, args []string) int {
	subargs := args
	switch {
	case len(args) > 0 && args[0] == "fleet":
		subargs = args[1:]
	case len(args) > 0 && args[0] == "budget":
		return runBudget(rc, args[1:])
	case len(args) > 0 && (args[0] == "help" || args[0] == "-h" || args[0] == "--help"):
		fmt.Fprint(rc.Out, usageText)
		return 0
	}

	fs := tool.NewFlags(cmd.Name)
	asJSON := fs.Bool("json", weavecli.IsAgent(), "emit a bashy-resources-v1 envelope")
	operands, code := tool.Parse(rc, cmd, fs, subargs)
	if code >= 0 {
		return code
	}
	_ = operands

	fr, err := resources.CollectFleetResources(rc.Ctx)
	if err != nil {
		fmt.Fprintf(rc.Err, "resources fleet: %v\n", err)
		return 1
	}

	if *asJSON {
		b, err := json.MarshalIndent(fr, "", "  ")
		if err != nil {
			fmt.Fprintf(rc.Err, "resources fleet: %v\n", err)
			return 1
		}
		fmt.Fprintln(rc.Out, string(b))
		return 0
	}

	fmt.Fprint(rc.Out, resources.FormatTable(fr))
	return 0
}

// runBudget implements `resources budget`: the READ side of the local LLM
// meter. Budget is enforced on an agent and, until now, invisible to it — an
// agent that cannot see its remaining budget can only be cut off mid-thought,
// never choose to summarise early or hand off. It lands here rather than on a
// new top-level verb because `resources` is already the "what is left on this
// host" noun (see docs/orchestration-verb-consolidation-audit.md: 28
// orchestration verbs, 17 unjustified — bashy stays lean).
func runBudget(rc *tool.RunContext, args []string) int {
	fs := tool.NewFlags(cmd.Name)
	asJSON := fs.Bool("json", weavecli.IsAgent(), "emit a bashy-resources-v1 envelope")
	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}

	br := resources.CollectBudget(time.Time{}, operands)

	if *asJSON {
		b, err := json.MarshalIndent(br, "", "  ")
		if err != nil {
			fmt.Fprintf(rc.Err, "resources budget: %v\n", err)
			return 1
		}
		fmt.Fprintln(rc.Out, string(b))
		return 0
	}

	fmt.Fprint(rc.Out, resources.FormatBudgetTable(br))
	return 0
}
