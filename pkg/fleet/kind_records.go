package fleet

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/assetring"
)

// The write surface of the kinds that carry typed flags. Each is the part of
// `show` / `add` / `set` that truly differs per noun; the verbs themselves are
// built once in kind_verbs.go.

const (
	helpDisplay   = "human-facing label"
	helpHidden    = "omit this tool from default listings"
	helpProvider  = "anthropic | openai | gemini | openai-compat | ollama"
	helpModelKind = "subscription | api | local"
)

func toolRecord() *recordSpec {
	return newRecordSpec(typedRecord[Tool]{
		Get:     (*Catalog).Tool,
		Name:    func(t *Tool) *string { return &t.Name },
		Ring:    func(t Tool) assetring.Ring { return t.Ring },
		Aliases: func(t *Tool) *[]string { return &t.Aliases },
		Parse: func(fallback string, body []byte) ([]Tool, error) {
			t, err := ParseTool(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []Tool{t}, nil
		},
		Save:      (*Catalog).SaveTool,
		ShowShort: "Print a tool's definition",
		AddDoc: verbDoc{
			Use:   "add (<name> --set path=value | <file>|-)",
			Short: "Add or import a tool definition into the local store",
		},
		SetDoc: verbDoc{
			Short: "Modify a tool definition",
			Long:  "Modify a tool definition. Entries from a lower ring gain a sparse local overlay.",
		},
		NameFlag: "store under this name instead of the document's own",
		AliasDoc: aliasDoc{AddAlias: "add an alias (repeatable)", RmAlias: "drop an alias (repeatable)"},
		Flags: []kindFlag{
			strFlag("binary", "cli.binary", "", "the executable to run", func(t *Tool) *string { return &t.CLI.Binary }),
			strFlag("exec", "cli.launch.exec", "", "launch template; {prompt} and {model} are substituted", func(t *Tool) *string { return &t.CLI.Launch.Exec }),
			strFlag("display", "display", "", helpDisplay, func(t *Tool) *string { return &t.Display }),
			boolFlag("hidden", "hidden", helpHidden, helpHidden, func(t *Tool) *bool { return &t.Hidden }),
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, t Tool, set bool) error {
			if !set {
				fmt.Fprintf(cmd.OutOrStdout(), "%s (%s)\n", t.Name, t.Kind)
				return nil
			}
			if t.CLI.Launch.Exec != "" && !t.TakesModel() {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s has no %s placeholder, so agents bound to it cannot select a model\n", t.Name, ModelToken)
			}
			fmt.Fprintln(cmd.OutOrStdout(), t.Name)
			return nil
		},
	})
}

func modelRecord() *recordSpec {
	return newRecordSpec(typedRecord[Model]{
		Get:     (*Catalog).Model,
		Name:    func(m *Model) *string { return &m.Name },
		Ring:    func(m Model) assetring.Ring { return m.Ring },
		Aliases: func(m *Model) *[]string { return &m.Aliases },
		Parse: func(fallback string, body []byte) ([]Model, error) {
			m, err := ParseModel(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []Model{m}, nil
		},
		Save:      (*Catalog).SaveModel,
		ShowShort: "Print a model's definition",
		AddDoc: verbDoc{
			Use:   "add (<name> --provider P --kind K | <file>|-)",
			Short: "Add an inference backend to the local store",
			Long: "Add an inference backend to the local store.\n\n" +
				"Name a model for the exact version it is (`opus5`), not the family it\n" +
				"belongs to (`opus`). Declare --family and --version and the catalog will\n" +
				"point the bare family name at whichever version is newest — so the alias\n" +
				"follows the releases and the name in a record never changes meaning.\n\n" +
				"--band is the model's capability peg, 1 (basic) to 5 (frontier), measured\n" +
				"across providers rather than taken from the vendor's own tier ladder.",
			Example: "  bashy model add opus5 --family opus --version 5 --band 3 \\\n" +
				"      --provider anthropic --kind subscription --upstream claude-opus-5\n" +
				"  bashy model add ./deepseek.yaml",
		},
		SetDoc:   verbDoc{Short: "Modify an inference backend"},
		AliasDoc: aliasDoc{Add: "an additional name (repeatable)", AddAlias: "add an alias (repeatable)", RmAlias: "drop an alias (repeatable)"},
		Flags: []kindFlag{
			strFlag("provider", "provider", helpProvider, helpProvider, func(m *Model) *string { return &m.Provider }),
			strFlag("kind", "kind", helpModelKind, helpModelKind, func(m *Model) *string { return &m.Kind }),
			strFlag("upstream", "model", "provider-side model id (the value passed to --model)", "provider-side model id", func(m *Model) *string { return &m.UpstreamID }),
			strFlag("base-url", "base_url", "API base URL", "API base URL", func(m *Model) *string { return &m.BaseURL }),
			strFlag("api-key-ref", "api_key_ref", "vault key name; never an inline secret", "vault key name; never an inline secret", func(m *Model) *string { return &m.APIKeyRef }),
			strFlag("display", "display", helpDisplay, helpDisplay, func(m *Model) *string { return &m.Display }),
			strFlag("family", "family", "product line; the bare family name floats to its newest version", "", func(m *Model) *string { return &m.Family }),
			strFlag("version", "version", "version within the family, e.g. 4.9", "", func(m *Model) *string { return &m.Version }),
			intFlag("band", "band", "capability peg 1-5, normalized across providers", "capability peg 1-5, normalized across providers", func(m *Model) *int { return &m.Band }),
			strFlag("band-source", "band_source", "evidence for the capability band", "evidence for the capability band", func(m *Model) *string { return &m.BandSource }),
			listFlag("id", "tool_ids", "tool-specific model id as <tool>=<upstream> (repeatable)", "tool-specific model id as <tool>=<upstream> (repeatable)", applyIDs),
			floatFlag("quality", "quality", "capability prior in [0,1]; the router's quality term", "capability prior in [0,1]; the router's quality term", func(m *Model) *float64 { return &m.Quality }),
			int64Flag("cost-micro", "cost_micro", "relative per-turn cost; the router's cost term", "relative per-turn cost; the router's cost term", func(m *Model) *int64 { return &m.CostMicro }),
		},
		Saved: func(cmd *cobra.Command, cat *Catalog, m Model, set bool) error {
			if set {
				fmt.Fprintln(cmd.OutOrStdout(), m.Name)
				return nil
			}
			chk := cat.VerifyModel(m.Name, Probes(nil))
			fmt.Fprintf(cmd.OutOrStdout(), "%s → %s\n", m.Name, m.Target())
			if !chk.OK {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", chk.Reason)
			}
			return nil
		},
	})
}

func applyIDs(m *Model, ids []string) error {
	for _, assignment := range ids {
		tool, upstream, ok := strings.Cut(assignment, "=")
		if !ok || tool == "" || upstream == "" {
			return fmt.Errorf("fleet: --id needs <tool>=<upstream>, got %q", assignment)
		}
		if m.ToolIDs == nil {
			m.ToolIDs = make(map[string]string)
		}
		m.ToolIDs[tool] = upstream
	}
	return nil
}

func agentRecord() *recordSpec {
	return newRecordSpec(typedRecord[Agent]{
		Get:     (*Catalog).Agent,
		Name:    func(a *Agent) *string { return &a.Name },
		Ring:    func(a Agent) assetring.Ring { return a.Ring },
		Aliases: func(a *Agent) *[]string { return &a.Aliases },
		// The claim covers the explicit nickname too — an operator naming an
		// agent `codex` would otherwise shadow the tool of that name. Derived
		// names are absent here by construction: they are never persisted, so
		// there is nothing of ours to re-claim.
		Claims: func(a Agent) []string { return append(append([]string{}, a.Aliases...), a.Nick) },
		Parse: func(fallback string, body []byte) ([]Agent, error) {
			file, err := ParseAgentFile(fallback, body, nil)
			return file.Agents, err
		},
		Save:      (*Catalog).SaveAgent,
		ShowShort: "Print an agent's binding",
		ShowLong: "Print an agent's binding. <name> may be a nickname, an alias, or a bare tool:model.\n\n" +
			"The summary includes the agent's place on the band ladder: the effective band,\n" +
			"the DERIVED band (highest n whose gates G1..Gn all hold), any seed peg and\n" +
			"whether it still holds, each failed gate condition up to the next band, the\n" +
			"code/manage/judge ratings (r ± RD, events), and each certificate's validity.\n" +
			"--json and --yaml print the stored record, which never contains a band.",
		Summary: showAgentSummary,
		// An agent's asset blob is the envelope, not the bare agent — that is
		// the shape the store holds and the control plane serves.
		YAMLBlob: func(a Agent) any { return AgentFile{Agents: []Agent{a}} },
		AddDoc: verbDoc{
			Use:   "add (<nickname> --tool T --model M | <file>|-)",
			Short: "Mint an agent: a named tool:model binding",
			Long: "Mint an agent: a named tool:model binding.\n\n" +
				"Give a nickname with --tool and --model to build the binding from flags,\n" +
				"or give a path (or - for stdin) to import an agent asset document.\n\n" +
				"A nickname is an alias for a binding, not an identity of its own:\n" +
				"`007` and `smarty` may both name claude:fable5, and both resolve to the\n" +
				"same capability-matrix row.",
			Example: "  bashy agent add 007 --tool codex --model deepseek-v4 --alias smarty\n" +
				"  bashy agent add ./conductor.yaml",
		},
		SetDoc: verbDoc{
			Short: "Modify an agent's binding or nicknames",
			Long: "Modify an agent's binding or nicknames.\n\n" +
				"An agent from a lower ring gains a sparse host-local overlay on first\n" +
				"modification. The lower catalog remains readable and unchanged.",
			Example: "  bashy agent set 007 --model opus --add-alias bond",
		},
		AliasDoc: aliasDoc{Add: "an additional nickname (repeatable)", AddAlias: "add a nickname (repeatable)", RmAlias: "drop a nickname (repeatable)"},
		AddCheck: func(a Agent) error {
			if a.Tool == "" || a.Model == "" {
				return fmt.Errorf("fleet: minting %q needs both --tool and --model (an agent always names both)", a.Name)
			}
			return nil
		},
		Flags: []kindFlag{
			strFlag("tool", "tool", "the agentic CLI half of the binding", "the agentic CLI half of the binding", func(a *Agent) *string { return &a.Tool }),
			strFlag("model", "model", "the inference-backend half of the binding", "the inference-backend half of the binding", func(a *Agent) *string { return &a.Model }),
			strFlag("display", "display", helpDisplay, helpDisplay, func(a *Agent) *string { return &a.Display }),
			strFlag("description", "description", "what this agent is for", "what this agent is for", func(a *Agent) *string { return &a.Description }),
			strFlag("nick", "nick", "the agent's human name; empty means one is assigned from the binding", "the agent's human name; empty means one is assigned from the binding", func(a *Agent) *string { return &a.Nick }),
			boolFlag("ephemeral", "ephemeral", "mark this as a one-task agent", "mark this as a one-task agent", func(a *Agent) *bool { return &a.Ephemeral }),
			recFlag("sprint", "", "own this ephemeral seat by sprint UUID (defaults to managed sprint context)", "", defineString,
				func(a *Agent, sprint string) error { MintAgentLifecycle(a, sprint); return nil }).finish(),
		},
		Saved: func(cmd *cobra.Command, cat *Catalog, a Agent, _ bool) error {
			return reportAgentSaved(cmd, cat, a)
		},
	})
}
