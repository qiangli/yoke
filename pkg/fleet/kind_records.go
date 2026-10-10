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
		Save: (*Catalog).SaveAgent,
		// A new binding to a retired tool or model is refused unless asked
		// for explicitly (plan section I).
		BeforeAdd: checkNewAgentRetirement,
		AddFlags: func(c *cobra.Command) {
			c.Flags().Bool("allow-retired", false, "allow a binding to retired entries")
		},
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

func commandRecord() *recordSpec {
	return newRecordSpec(typedRecord[Command]{
		Get:     (*Catalog).Command,
		Name:    func(r *Command) *string { return &r.Name },
		Ring:    func(r Command) assetring.Ring { return r.Ring },
		Aliases: func(r *Command) *[]string { return &r.Aliases },
		Parse: func(fallback string, body []byte) ([]Command, error) {
			r, err := ParseCommand(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []Command{r}, nil
		},
		Save: (*Catalog).SaveCommand,
		// The shadow refusal and the reserved words live in checkCommand, which
		// Save and every overlay write both answer to; --force never reaches it.
		Check:     (*Catalog).checkCommand,
		ShowShort: "Print a registered command's record",
		AddDoc: verbDoc{
			Use:     "add (<name> --set path=value… | <file>|-)",
			Aliases: []string{"register"}, // the verb's own name for what it does: a fence runner is registered, then named
			Short:   "Register a command in the local store",
			Long: "Register a command in the local store, from --set paths on an empty record\n" +
				"or from a YAML record file (- = stdin). Exactly one of exec / download /\n" +
				"script is required; `commands schema` lists every path.\n\n" +
				"A name that already resolves to a builtin, applet, verb, managed external,\n" +
				"alias or skill is REFUSED — there is no --force for that: a registered\n" +
				"command may shadow a PATH program, never a command bashy ships.",
			Example: "  bashy commands add gl --set script='git log --oneline -n \"${1:-10}\"' --set effects.0=read\n" +
				"  bashy commands add paint --set script='echo \"$@\"' --set effects.0=pure \\\n" +
				"      --set args.positionals.0.name=color --set args.positionals.0.enum.0=red --set args.positionals.0.enum.1=blue \\\n" +
				"      --set args.flags.0.name=count --set args.flags.0.type=int --set args.flags.0.required=true\n" +
				"  bashy commands add jqs --set exec.0=/usr/local/bin/jq --set exec.1=-S\n" +
				"  bashy commands add witr --set download.github=owner/repo --set download.version=v0.3.3 \\\n" +
				"      --set download.sha256.linux/amd64=<hex>\n" +
				"  bashy commands add ./witr.yaml",
		},
		SetDoc: verbDoc{
			Short: "Modify a registered command",
			Long:  "Modify a registered command. An entry from a shared ring gains a sparse local overlay.",
		},
		NameFlag: "store under this name instead of the document's own",
		ForceDoc: "take a name that already belongs to another registered command (never a builtin)",
		AliasDoc: aliasDoc{AddAlias: "add an alias (repeatable)", RmAlias: "drop an alias (repeatable)"},
		Flags: []kindFlag{
			boolFlag("hidden", "hidden", "omit this command from default listings", "omit this command from default listings", func(r *Command) *bool { return &r.Hidden }),
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, r Command, set bool) error {
			if set {
				fmt.Fprintln(cmd.OutOrStdout(), r.Name)
				return nil
			}
			r.applyDefaults()
			fmt.Fprintf(cmd.OutOrStdout(), "%s (%s: %s)\n", r.Name, r.Mode(), strings.Join(r.Effects, ","))
			return nil
		},
	})
}

func appRecord() *recordSpec {
	return newRecordSpec(typedRecord[App]{
		Get:  (*Catalog).App,
		Name: func(a *App) *string { return &a.Name },
		Ring: func(a App) assetring.Ring { return a.Ring },
		Parse: func(fallback string, body []byte) ([]App, error) {
			a, err := ParseApp(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []App{a}, nil
		},
		Save:      (*Catalog).SaveApp,
		Check:     (*Catalog).checkApp,
		ShowShort: "Print a registered app's record",
		AddDoc: verbDoc{
			Use:     "add (<name> --port N [flags] | <file>|-)",
			Aliases: []string{"register"},
			Short:   "Register a local web app as a console tile",
			Long: "Register a local web server as an Apps tile, proxied at /<name>/ to\n" +
				"127.0.0.1:<port>. The record is read as data: the console never runs\n" +
				"anything to discover a registered app. A name the console already uses\n" +
				"(a builtin panel, an atlas surface, a reserved mount) is refused.\n\n" +
				AppsHint + ".",
			Example: "  bashy app add jupyter --port 8888 --label Jupyter --icon 📓 --start jupyter --start lab\n" +
				"  bashy app add ./grafana.yaml",
		},
		SetDoc: verbDoc{
			Short: "Modify a registered app",
			Long:  "Modify a registered app. An entry from a shared ring gains a sparse local overlay.",
		},
		ForceDoc: "take a name that already belongs to another registered app",
		Flags: []kindFlag{
			intFlag("port", "port", "loopback port the app listens on", "loopback port the app listens on", func(a *App) *int { return &a.Port }),
			strFlag("label", "label", "tile label (default: the name)", "tile label (default: the name)", func(a *App) *string { return &a.Label }),
			strFlag("icon", "icon", "one emoji, or SVG path data on a 24 grid", "one emoji, or SVG path data on a 24 grid", func(a *App) *string { return &a.Icon }),
			strFlag("tip", "tip", "tile tooltip", "tile tooltip", func(a *App) *string { return &a.Tip }),
			listFlag("start", "start", "start-hint argv element shown when the app is down (repeatable; never run)", "start-hint argv element shown when the app is down (repeatable; never run)",
				func(a *App, argv []string) error { a.Start = argv; return nil }),
			strFlag("auth", "auth", "auth tier: system (default), public or custom", "auth tier: system (default), public or custom", func(a *App) *string { return &a.Auth }),
			strFlag("login-path", "login_path", "custom auth only: app-relative login path", "custom auth only: app-relative login path", func(a *App) *string { return &a.LoginPath }),
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, a App, set bool) error {
			if set {
				fmt.Fprintln(cmd.OutOrStdout(), a.Name)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "%s -> /%s/ (127.0.0.1:%d)\n", a.Name, a.Name, a.Port)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "note: "+AppsHint)
			return nil
		},
	})
}

func personRecord() *recordSpec {
	return newRecordSpec(typedRecord[Person]{
		Get:     (*Catalog).Person,
		Name:    func(p *Person) *string { return &p.Handle },
		Ring:    func(p Person) assetring.Ring { return p.Ring },
		Aliases: func(p *Person) *[]string { return &p.Aliases },
		Parse: func(fallback string, body []byte) ([]Person, error) {
			p, err := ParsePerson(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []Person{p}, nil
		},
		Save: (*Catalog).SavePerson,
		// Adding is not replacing: a handle that already names someone is
		// refused unless --force says so.
		BeforeAdd: func(cmd *cobra.Command, c *Catalog, p Person) error {
			if force, _ := cmd.Flags().GetBool("force"); force {
				return nil
			}
			return c.refuseExistingPerson(p.Handle)
		},
		ShowShort: "Print a person's record",
		AddDoc: verbDoc{
			Use:   "add (<handle> [flags] | <file>|-)",
			Short: "Add a human principal",
			Long: "Add a human principal.\n\n" +
				"Account names are recorded per host, never globally. Assuming the local\n" +
				"$USER exists on a remote machine is the most common way a cross-host\n" +
				"reach fails, so an unbound host makes `whois` say it is guessing.",
			Example: "  bashy person add alice --display \"Alice\" --email alice@example.com --os-user host-a=alice --os-user host-b=al",
		},
		SetDoc:   verbDoc{Short: "Modify a human principal"},
		ForceDoc: "replace an existing person, or take a name that already belongs to someone else",
		AliasDoc: aliasDoc{Add: "an additional name (repeatable)", AddAlias: "add a name (repeatable)", RmAlias: "drop a name (repeatable)"},
		Flags: []kindFlag{
			strFlag("display", "display", helpDisplay, helpDisplay, func(p *Person) *string { return &p.Display }),
			strFlag("email", "email", "account email; authoritative identity when paired", "account email", func(p *Person) *string { return &p.Email }),
			strFlag("default-os-user", "default_os_user", "account name on hosts with no explicit binding", "account name on hosts with no explicit binding", func(p *Person) *string { return &p.DefaultOSUser }),
			listFlag("os-user", "os_users", "host=account binding (repeatable)", "host=account binding (repeatable)", applyOSUsers),
			listFlag("host", "hosts", "a host this person owns (repeatable)", "",
				func(p *Person, hosts []string) error { p.Hosts = mergeAliases(p.Hosts, hosts, nil); return nil }),
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, p Person, _ bool) error {
			fmt.Fprintln(cmd.OutOrStdout(), p.Handle)
			return nil
		},
	})
}

// applyOSUsers merges repeatable host=account bindings into a person's
// per-host account map.
func applyOSUsers(p *Person, bindings []string) error {
	for _, b := range bindings {
		host, user, ok := strings.Cut(b, "=")
		if !ok || host == "" || user == "" {
			return fmt.Errorf("fleet: --os-user wants host=account, got %q", b)
		}
		if p.OSUsers == nil {
			p.OSUsers = map[string]string{}
		}
		p.OSUsers[host] = user
	}
	return nil
}

func hostRecord() *recordSpec {
	return newRecordSpec(typedRecord[Host]{
		Get:     (*Catalog).Host,
		Name:    func(h *Host) *string { return &h.Name },
		Ring:    func(h Host) assetring.Ring { return h.Ring },
		Aliases: func(h *Host) *[]string { return &h.Aliases },
		Parse: func(fallback string, body []byte) ([]Host, error) {
			h, err := ParseHost(fallback, body, nil)
			if err != nil {
				return nil, err
			}
			return []Host{h}, nil
		},
		Save:      (*Catalog).SaveHost,
		ShowShort: "Print a host's reach record",
		AddDoc: verbDoc{
			Use:   "add (<name> [flags] | <file>|-)",
			Short: "Add a static reach alias for a machine",
			Long: "Add a static reach alias for a machine.\n\n" +
				"A host entry carries only what discovery cannot: the address on a network\n" +
				"with no mDNS, a non-default ssh port, the account name to use there.",
			Example: "  bashy host add lab --address lab.example.test --ssh-user me --ssh-port 2222",
		},
		SetDoc:   verbDoc{Short: "Modify a host's reach record"},
		AliasDoc: aliasDoc{Add: "an additional name (repeatable)", AddAlias: "add a name (repeatable)", RmAlias: "drop a name (repeatable)"},
		NameFlag: "store under this name instead of the document's own",
		Flags: []kindFlag{
			strFlag("display", "display", helpDisplay, helpDisplay, func(h *Host) *string { return &h.Display }),
			strFlag("address", "address", "where the machine answers: a DNS name or address literal", "where the machine answers: a DNS name or address literal", func(h *Host) *string { return &h.Address }),
			strFlag("ssh-user", "ssh_user", "account name on this host", "account name on this host", func(h *Host) *string { return &h.SSHUser }),
			intFlag("ssh-port", "ssh_port", "non-default ssh port", "non-default ssh port", func(h *Host) *int { return &h.SSHPort }),
			strFlag("lan-endpoint", "lan_endpoint", "service URL reachable only from the same network", "service URL reachable only from the same network", func(h *Host) *string { return &h.LANEndpoint }),
			strFlag("notes", "notes", "free-form notes", "free-form notes", func(h *Host) *string { return &h.Notes }),
		},
		Saved: func(cmd *cobra.Command, _ *Catalog, h Host, _ bool) error {
			fmt.Fprintln(cmd.OutOrStdout(), h.Name)
			return nil
		},
	})
}
