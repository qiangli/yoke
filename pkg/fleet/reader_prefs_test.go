package fleet

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReaderPrefs_BashyDefault(t *testing.T) {
	resolved, err := ResolveReaderPrefs(ReaderPrefs{}, "", nil, nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.View != ViewText {
		t.Errorf("View = %q, want %q", resolved.View, ViewText)
	}
	if resolved.Help != HelpClassic {
		t.Errorf("Help = %q, want %q", resolved.Help, HelpClassic)
	}
	if resolved.Dialect != "bash" {
		t.Errorf("Dialect = %q, want %q", resolved.Dialect, "bash")
	}
	if resolved.BudgetBytes != DefaultBudgetBytes {
		t.Errorf("BudgetBytes = %d, want %d", resolved.BudgetBytes, DefaultBudgetBytes)
	}
}

func TestReaderPrefs_BandPrecedence(t *testing.T) {
	tests := []struct {
		name     string
		band     int
		wantHelp string
	}{
		{"band 0 unpegged", 0, HelpClassic},
		{"band 1 low", 1, HelpScript},
		{"band 2 low", 2, HelpScript},
		{"band 3 medium", 3, HelpSkill},
		{"band 4 high", 4, HelpSkill},
		{"band 5 frontier", 5, HelpSkill},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolved, err := ResolveReaderPrefs(ReaderPrefs{}, "", nil, nil, tt.band)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resolved.Help != tt.wantHelp {
				t.Errorf("band %d: Help = %q, want %q", tt.band, resolved.Help, tt.wantHelp)
			}
			// Other fields inherit from bashy default
			if resolved.View != ViewText {
				t.Errorf("View = %q, want %q", resolved.View, ViewText)
			}
			if resolved.Dialect != "bash" {
				t.Errorf("Dialect = %q, want %q", resolved.Dialect, "bash")
			}
			if resolved.BudgetBytes != DefaultBudgetBytes {
				t.Errorf("BudgetBytes = %d, want %d", resolved.BudgetBytes, DefaultBudgetBytes)
			}
		})
	}
}

func TestReaderPrefs_ToolPrecedence(t *testing.T) {
	tool := &Tool{
		Name: "claude",
		Prefs: ReaderPrefs{
			View:        ViewJSON,
			Help:        HelpMCP,
			Dialect:     DialectPWSH,
			BudgetBytes: 30000,
			Defaults: map[string]map[string]any{
				"*": {"no_input": true},
			},
		},
	}

	// Tool should override band 1 (which would otherwise default Help to "script") and bashy default
	resolved, err := ResolveReaderPrefs(ReaderPrefs{}, "", nil, tool, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.View != ViewJSON {
		t.Errorf("View = %q, want %q", resolved.View, ViewJSON)
	}
	if resolved.Help != HelpMCP {
		t.Errorf("Help = %q, want %q", resolved.Help, HelpMCP)
	}
	if resolved.Dialect != DialectPWSH {
		t.Errorf("Dialect = %q, want %q", resolved.Dialect, DialectPWSH)
	}
	if resolved.BudgetBytes != 30000 {
		t.Errorf("BudgetBytes = %d, want 30000", resolved.BudgetBytes)
	}
	if opt, ok := resolved.Defaults["*"]["no_input"]; !ok || opt != true {
		t.Errorf("Defaults[*] = %v, want no_input: true", resolved.Defaults["*"])
	}
}

func TestReaderPrefs_AgentPrecedence(t *testing.T) {
	tool := &Tool{
		Name: "claude",
		Prefs: ReaderPrefs{
			View:        ViewJSON,
			Help:        HelpMCP,
			Dialect:     DialectPWSH,
			BudgetBytes: 30000,
			Defaults: map[string]map[string]any{
				"*": {"no_input": true, "timeout": "30s"},
			},
		},
	}

	agent := &Agent{
		Name: "coder",
		Prefs: ReaderPrefs{
			View:        ViewYAML,
			Help:        HelpSkill,
			BudgetBytes: 25000,
			Defaults: map[string]map[string]any{
				"*":  {"timeout": "10s"},
				"rm": {"dry_run": true},
			},
		},
	}

	resolved, err := ResolveReaderPrefs(ReaderPrefs{}, "", agent, tool, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Agent overrides View, Help, BudgetBytes
	if resolved.View != ViewYAML {
		t.Errorf("View = %q, want %q", resolved.View, ViewYAML)
	}
	if resolved.Help != HelpSkill {
		t.Errorf("Help = %q, want %q", resolved.Help, HelpSkill)
	}
	if resolved.BudgetBytes != 25000 {
		t.Errorf("BudgetBytes = %d, want 25000", resolved.BudgetBytes)
	}
	// Dialect inherits from tool
	if resolved.Dialect != DialectPWSH {
		t.Errorf("Dialect = %q, want %q", resolved.Dialect, DialectPWSH)
	}

	// Defaults merge: * has no_input from tool, timeout from agent (10s); rm from agent
	starOpts := resolved.Defaults["*"]
	if starOpts["no_input"] != true {
		t.Errorf("Defaults[*][no_input] = %v, want true", starOpts["no_input"])
	}
	if starOpts["timeout"] != "10s" {
		t.Errorf("Defaults[*][timeout] = %v, want 10s", starOpts["timeout"])
	}
	if rmOpts := resolved.Defaults["rm"]; rmOpts["dry_run"] != true {
		t.Errorf("Defaults[rm][dry_run] = %v, want true", rmOpts["dry_run"])
	}
}

func TestReaderPrefs_EnvPrecedence(t *testing.T) {
	tool := &Tool{
		Name: "claude",
		Prefs: ReaderPrefs{
			Dialect: DialectPWSH,
		},
	}
	agent := &Agent{
		Name: "coder",
		Prefs: ReaderPrefs{
			View:        ViewYAML,
			Help:        HelpSkill,
			BudgetBytes: 25000,
		},
	}

	envJSON := `{"view":"md","budget_bytes":15000,"defaults":{"graph *":{"depth":1}},"custom_flag":true}`

	resolved, err := ResolveReaderPrefs(ReaderPrefs{}, envJSON, agent, tool, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Env overrides agent View and BudgetBytes
	if resolved.View != ViewMD {
		t.Errorf("View = %q, want %q", resolved.View, ViewMD)
	}
	if resolved.BudgetBytes != 15000 {
		t.Errorf("BudgetBytes = %d, want 15000", resolved.BudgetBytes)
	}
	// Help inherits from agent
	if resolved.Help != HelpSkill {
		t.Errorf("Help = %q, want %q", resolved.Help, HelpSkill)
	}
	// Dialect inherits from tool
	if resolved.Dialect != DialectPWSH {
		t.Errorf("Dialect = %q, want %q", resolved.Dialect, DialectPWSH)
	}
	// Defaults from env
	if d := resolved.Defaults["graph *"]; d["depth"] != float64(1) {
		t.Errorf("Defaults[graph *][depth] = %v, want 1", d["depth"])
	}
	// Underscore keys in Extra preserved
	if resolved.Extra["custom_flag"] != true {
		t.Errorf("Extra[custom_flag] = %v, want true", resolved.Extra["custom_flag"])
	}
}

func TestReaderPrefs_ExplicitFlagPrecedence(t *testing.T) {
	tool := &Tool{
		Name: "claude",
		Prefs: ReaderPrefs{
			Dialect: DialectPWSH,
		},
	}
	agent := &Agent{
		Name: "coder",
		Prefs: ReaderPrefs{
			View:        ViewYAML,
			Help:        HelpSkill,
			BudgetBytes: 25000,
		},
	}
	envJSON := `{"view":"md","budget_bytes":15000}`

	explicit := ReaderPrefs{
		View:        ViewCanonical,
		BudgetBytes: 8000,
	}

	resolved, err := ResolveReaderPrefs(explicit, envJSON, agent, tool, 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Explicit overrides Env
	if resolved.View != ViewCanonical {
		t.Errorf("View = %q, want %q", resolved.View, ViewCanonical)
	}
	if resolved.BudgetBytes != 8000 {
		t.Errorf("BudgetBytes = %d, want 8000", resolved.BudgetBytes)
	}
	// Non-overridden fields inherit down the chain
	if resolved.Help != HelpSkill {
		t.Errorf("Help = %q, want %q (from agent)", resolved.Help, HelpSkill)
	}
	if resolved.Dialect != DialectPWSH {
		t.Errorf("Dialect = %q, want %q (from tool)", resolved.Dialect, DialectPWSH)
	}
}

func TestReaderPrefs_AllPrecedenceLevelsTogether(t *testing.T) {
	// Level 5: Band 2 -> sets Help="script"
	band := 2

	// Level 4: Tool -> sets Dialect="pwsh", BudgetBytes=32000, Defaults={"*": {"no_input": true, "timeout": "30s"}}
	tool := &Tool{
		Name: "test-tool",
		Prefs: ReaderPrefs{
			Dialect:     DialectPWSH,
			BudgetBytes: 32000,
			Defaults: map[string]map[string]any{
				"*": {"no_input": true, "timeout": "30s"},
			},
		},
	}

	// Level 3: Agent -> sets Help="skill", Defaults={"*": {"timeout": "10s"}, "git *": {"depth": 2}}
	agent := &Agent{
		Name: "test-agent",
		Prefs: ReaderPrefs{
			Help: HelpSkill,
			Defaults: map[string]map[string]any{
				"*":     {"timeout": "10s"},
				"git *": {"depth": 2},
			},
		},
	}

	// Level 2: Env -> sets View="yaml", Defaults={"git *": {"depth": 1}, "rm": {"dry_run": true}}
	envJSON := `{
		"view": "yaml",
		"defaults": {
			"git *": {"depth": 1},
			"rm": {"dry_run": true}
		}
	}`

	// Level 1: Explicit -> sets BudgetBytes=16000, Defaults={"rm": {"confirm": true}}
	explicit := ReaderPrefs{
		BudgetBytes: 16000,
		Defaults: map[string]map[string]any{
			"rm": {"confirm": true},
		},
	}

	resolved, err := ResolveReaderPrefs(explicit, envJSON, agent, tool, band)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// 1. View: explicitly unset -> from env ("yaml")
	if resolved.View != ViewYAML {
		t.Errorf("View = %q, want %q", resolved.View, ViewYAML)
	}

	// 2. Help: explicitly unset -> env unset -> from agent ("skill", overrides tool and band)
	if resolved.Help != HelpSkill {
		t.Errorf("Help = %q, want %q", resolved.Help, HelpSkill)
	}

	// 3. Dialect: explicitly unset -> env unset -> agent unset -> from tool ("pwsh", overrides bashy default "bash")
	if resolved.Dialect != DialectPWSH {
		t.Errorf("Dialect = %q, want %q", resolved.Dialect, DialectPWSH)
	}

	// 4. BudgetBytes: from explicit (16000, overrides env 0, agent 0, tool 32000, bashy default 40960)
	if resolved.BudgetBytes != 16000 {
		t.Errorf("BudgetBytes = %d, want 16000", resolved.BudgetBytes)
	}

	// 5. Defaults:
	//   "*" from tool has no_input: true; agent overrides timeout: "10s"
	star := resolved.Defaults["*"]
	if star["no_input"] != true {
		t.Errorf("Defaults[*][no_input] = %v, want true", star["no_input"])
	}
	if star["timeout"] != "10s" {
		t.Errorf("Defaults[*][timeout] = %v, want 10s", star["timeout"])
	}

	//   "git *" from agent had depth: 2; env overrides depth: 1
	git := resolved.Defaults["git *"]
	if git["depth"] != float64(1) {
		t.Errorf("Defaults[git *][depth] = %v, want 1", git["depth"])
	}

	//   "rm" from env had dry_run: true; explicit adds confirm: true
	rm := resolved.Defaults["rm"]
	if rm["dry_run"] != true {
		t.Errorf("Defaults[rm][dry_run] = %v, want true", rm["dry_run"])
	}
	if rm["confirm"] != true {
		t.Errorf("Defaults[rm][confirm] = %v, want true", rm["confirm"])
	}
}

func TestReaderPrefs_InvalidEnv(t *testing.T) {
	tests := []struct {
		name    string
		envJSON string
		wantErr bool
	}{
		{"empty string", "", false},
		{"whitespace string", "   \t\n", false},
		{"valid empty object", "{}", false},
		{"valid object", `{"view":"json"}`, false},
		{"syntax error unclosed", `{"view": "json"`, true},
		{"syntax error bare word", `{view: json}`, true},
		{"array instead of object", `[1, 2, 3]`, true},
		{"number instead of object", `42`, true},
		{"string instead of object", `"hello"`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ResolveReaderPrefs(ReaderPrefs{}, tt.envJSON, nil, nil, 0)
			if (err != nil) != tt.wantErr {
				t.Errorf("ResolveReaderPrefs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && err != nil && !strings.Contains(err.Error(), EnvBashyPrefs) {
				t.Errorf("error %q should mention %s", err.Error(), EnvBashyPrefs)
			}
		})
	}
}

func TestReaderPrefs_EmptyValuesInherit(t *testing.T) {
	// A higher layer with empty/zero values should inherit lower layer values
	lower := ReaderPrefs{
		View:        ViewJSON,
		Help:        HelpScript,
		Dialect:     DialectPWSH,
		BudgetBytes: 20000,
		Defaults: map[string]map[string]any{
			"*": {"no_input": true},
		},
		Extra: map[string]any{
			"custom_a": "alpha",
		},
	}

	higher := ReaderPrefs{
		View:        "",
		Help:        "",
		Dialect:     "",
		BudgetBytes: 0,
		Defaults:    nil,
		Extra:       nil,
	}

	merged := higher.Merge(lower)

	if merged.View != ViewJSON {
		t.Errorf("View = %q, want %q", merged.View, ViewJSON)
	}
	if merged.Help != HelpScript {
		t.Errorf("Help = %q, want %q", merged.Help, HelpScript)
	}
	if merged.Dialect != DialectPWSH {
		t.Errorf("Dialect = %q, want %q", merged.Dialect, DialectPWSH)
	}
	if merged.BudgetBytes != 20000 {
		t.Errorf("BudgetBytes = %d, want 20000", merged.BudgetBytes)
	}
	if merged.Defaults["*"]["no_input"] != true {
		t.Errorf("Defaults[*][no_input] = %v, want true", merged.Defaults["*"]["no_input"])
	}
	if merged.Extra["custom_a"] != "alpha" {
		t.Errorf("Extra[custom_a] = %v, want alpha", merged.Extra["custom_a"])
	}
}

func TestReaderPrefs_PreserveUnderscoreKeys(t *testing.T) {
	jsonPayload := `{
		"budget_bytes": 16000,
		"defaults": {
			"git *": {
				"no_pager": true,
				"max_count": 10
			}
		},
		"custom_model_tier": "pro",
		"enable_experimental_sink": true
	}`

	var p ReaderPrefs
	if err := json.Unmarshal([]byte(jsonPayload), &p); err != nil {
		t.Fatalf("Unmarshal error: %v", err)
	}

	if p.BudgetBytes != 16000 {
		t.Errorf("BudgetBytes = %d, want 16000", p.BudgetBytes)
	}

	gitOpts := p.Defaults["git *"]
	if gitOpts["no_pager"] != true {
		t.Errorf("no_pager = %v, want true", gitOpts["no_pager"])
	}
	if gitOpts["max_count"] != float64(10) {
		t.Errorf("max_count = %v, want 10", gitOpts["max_count"])
	}

	if p.Extra["custom_model_tier"] != "pro" {
		t.Errorf("custom_model_tier = %v, want pro", p.Extra["custom_model_tier"])
	}
	if p.Extra["enable_experimental_sink"] != true {
		t.Errorf("enable_experimental_sink = %v, want true", p.Extra["enable_experimental_sink"])
	}

	// Marshaling back should preserve underscore keys
	outBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal error: %v", err)
	}
	outStr := string(outBytes)

	for _, key := range []string{"budget_bytes", "no_pager", "max_count", "custom_model_tier", "enable_experimental_sink"} {
		if !strings.Contains(outStr, key) {
			t.Errorf("marshaled output %s missing underscore key %q", outStr, key)
		}
	}
}

func TestReaderPrefs_YAMLDeclaration(t *testing.T) {
	yamlData := `
name: test-agent
tool: claude
model: opus
prefs:
  view: yaml
  help: script
  dialect: pwsh
  budget_bytes: 16000
  defaults:
    "*":
      no_input: true
    "graph *":
      depth: 1
    "rm":
      dry_run: true
`
	var a Agent
	if err := yaml.Unmarshal([]byte(yamlData), &a); err != nil {
		t.Fatalf("yaml unmarshal error: %v", err)
	}

	if a.Prefs.View != ViewYAML {
		t.Errorf("Prefs.View = %q, want %q", a.Prefs.View, ViewYAML)
	}
	if a.Prefs.Help != HelpScript {
		t.Errorf("Prefs.Help = %q, want %q", a.Prefs.Help, HelpScript)
	}
	if a.Prefs.Dialect != DialectPWSH {
		t.Errorf("Prefs.Dialect = %q, want %q", a.Prefs.Dialect, DialectPWSH)
	}
	if a.Prefs.BudgetBytes != 16000 {
		t.Errorf("Prefs.BudgetBytes = %d, want 16000", a.Prefs.BudgetBytes)
	}
	if a.Prefs.Defaults["*"]["no_input"] != true {
		t.Errorf("Prefs.Defaults[*][no_input] = %v, want true", a.Prefs.Defaults["*"]["no_input"])
	}
	if a.Prefs.Defaults["graph *"]["depth"] != 1 {
		t.Errorf("Prefs.Defaults[graph *][depth] = %v, want 1", a.Prefs.Defaults["graph *"]["depth"])
	}
	if a.Prefs.Defaults["rm"]["dry_run"] != true {
		t.Errorf("Prefs.Defaults[rm][dry_run] = %v, want true", a.Prefs.Defaults["rm"]["dry_run"])
	}
}

func TestReaderPrefs_GlobDefaultsAlias(t *testing.T) {
	// In JSON, glob_defaults is accepted as alias for defaults
	jsonPayload := `{
		"glob_defaults": {
			"cat": {"number": true}
		}
	}`
	var p ReaderPrefs
	if err := json.Unmarshal([]byte(jsonPayload), &p); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if p.Defaults["cat"]["number"] != true {
		t.Errorf("Defaults[cat][number] = %v, want true", p.Defaults["cat"]["number"])
	}
	if p.GlobDefaults()["cat"]["number"] != true {
		t.Errorf("GlobDefaults()[cat][number] = %v, want true", p.GlobDefaults()["cat"]["number"])
	}

	// In YAML, glob_defaults is accepted as alias for defaults
	yamlData := `
glob_defaults:
  cat:
    number: true
`
	var pYAML ReaderPrefs
	if err := yaml.Unmarshal([]byte(yamlData), &pYAML); err != nil {
		t.Fatalf("yaml unmarshal error: %v", err)
	}
	if pYAML.Defaults["cat"]["number"] != true {
		t.Errorf("YAML Defaults[cat][number] = %v, want true", pYAML.Defaults["cat"]["number"])
	}
}

func TestReaderPrefs_ResolveFromOSEnv(t *testing.T) {
	t.Setenv(EnvBashyPrefs, `{"view":"yaml","budget_bytes":12345}`)

	resolved, err := ResolveReaderPrefsFromEnv(ReaderPrefs{}, nil, nil, 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if resolved.View != ViewYAML {
		t.Errorf("View = %q, want %q", resolved.View, ViewYAML)
	}
	if resolved.BudgetBytes != 12345 {
		t.Errorf("BudgetBytes = %d, want 12345", resolved.BudgetBytes)
	}
}

func TestReaderPrefsSources(t *testing.T) {
	t.Run("bashy default baseline sources", func(t *testing.T) {
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.View != ViewText || sources.View != SourceDefault {
			t.Errorf("View = %q (%s), want %q (%s)", resolved.View, sources.View, ViewText, SourceDefault)
		}
		if resolved.Help != HelpClassic || sources.Help != SourceDefault {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpClassic, SourceDefault)
		}
		if resolved.Dialect != "bash" || sources.Dialect != SourceDefault {
			t.Errorf("Dialect = %q (%s), want %q (%s)", resolved.Dialect, sources.Dialect, "bash", SourceDefault)
		}
		if resolved.BudgetBytes != DefaultBudgetBytes || sources.BudgetBytes != SourceDefault {
			t.Errorf("BudgetBytes = %d (%s), want %d (%s)", resolved.BudgetBytes, sources.BudgetBytes, DefaultBudgetBytes, SourceDefault)
		}
		if len(sources.Defaults) != 0 {
			t.Errorf("Defaults count = %d, want 0", len(sources.Defaults))
		}
		if sources.SourceFor("view") != SourceDefault {
			t.Errorf("SourceFor(view) = %q, want %q", sources.SourceFor("view"), SourceDefault)
		}
	})

	t.Run("band layer overrides help source", func(t *testing.T) {
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{Band: 1})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.Help != HelpScript || sources.Help != SourceBand {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpScript, SourceBand)
		}
		// Unpegged fields remain bashy defaults
		if sources.View != SourceDefault {
			t.Errorf("View source = %q, want %q", sources.View, SourceDefault)
		}
		if sources.Dialect != SourceDefault {
			t.Errorf("Dialect source = %q, want %q", sources.Dialect, SourceDefault)
		}
		if sources.BudgetBytes != SourceDefault {
			t.Errorf("BudgetBytes source = %q, want %q", sources.BudgetBytes, SourceDefault)
		}
	})

	t.Run("model layer overrides band", func(t *testing.T) {
		model := &Model{
			Name: "opus",
			Band: 2,
			Prefs: ReaderPrefs{
				Help: HelpMCP,
				Defaults: map[string]map[string]any{
					"llm": {"temp": 0.2},
				},
			},
		}
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			Model: model,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.Help != HelpMCP || sources.Help != SourceModel {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpMCP, SourceModel)
		}
		if src := sources.SourceForDefault("llm", "temp"); src != SourceModel {
			t.Errorf("SourceForDefault(llm, temp) = %q, want %q", src, SourceModel)
		}
	})

	t.Run("tool layer precedence and defaults tracking", func(t *testing.T) {
		tool := &Tool{
			Name: "claude",
			Prefs: ReaderPrefs{
				View:        ViewJSON,
				Help:        HelpMCP,
				Dialect:     DialectPWSH,
				BudgetBytes: 30000,
				Defaults: map[string]map[string]any{
					"*": {"no_input": true},
				},
			},
		}
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			Tool: tool,
			Band: 1, // band 1 would set help=script, but tool should override
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.View != ViewJSON || sources.View != SourceTool {
			t.Errorf("View = %q (%s), want %q (%s)", resolved.View, sources.View, ViewJSON, SourceTool)
		}
		if resolved.Help != HelpMCP || sources.Help != SourceTool {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpMCP, SourceTool)
		}
		if resolved.Dialect != DialectPWSH || sources.Dialect != SourceTool {
			t.Errorf("Dialect = %q (%s), want %q (%s)", resolved.Dialect, sources.Dialect, DialectPWSH, SourceTool)
		}
		if resolved.BudgetBytes != 30000 || sources.BudgetBytes != SourceTool {
			t.Errorf("BudgetBytes = %d (%s), want 30000 (%s)", resolved.BudgetBytes, sources.BudgetBytes, SourceTool)
		}
		if src := sources.SourceForDefault("*", "no_input"); src != SourceTool {
			t.Errorf("Defaults[*][no_input] source = %q, want %q", src, SourceTool)
		}
	})

	t.Run("agent layer precedence and default key merging", func(t *testing.T) {
		tool := &Tool{
			Name: "claude",
			Prefs: ReaderPrefs{
				View:        ViewJSON,
				Help:        HelpMCP,
				Dialect:     DialectPWSH,
				BudgetBytes: 30000,
				Defaults: map[string]map[string]any{
					"*": {"no_input": true, "timeout": "30s"},
				},
			},
		}
		agent := &Agent{
			Name: "coder",
			Prefs: ReaderPrefs{
				View:        ViewYAML,
				Help:        HelpSkill,
				BudgetBytes: 25000,
				Defaults: map[string]map[string]any{
					"*":  {"timeout": "10s"},
					"rm": {"dry_run": true},
				},
			},
		}
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			Agent: agent,
			Tool:  tool,
			Band:  1,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Agent overrides View, Help, BudgetBytes
		if resolved.View != ViewYAML || sources.View != SourceAgent {
			t.Errorf("View = %q (%s), want %q (%s)", resolved.View, sources.View, ViewYAML, SourceAgent)
		}
		if resolved.Help != HelpSkill || sources.Help != SourceAgent {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpSkill, SourceAgent)
		}
		if resolved.BudgetBytes != 25000 || sources.BudgetBytes != SourceAgent {
			t.Errorf("BudgetBytes = %d (%s), want 25000 (%s)", resolved.BudgetBytes, sources.BudgetBytes, SourceAgent)
		}
		// Dialect inherited from Tool
		if resolved.Dialect != DialectPWSH || sources.Dialect != SourceTool {
			t.Errorf("Dialect = %q (%s), want %q (%s)", resolved.Dialect, sources.Dialect, DialectPWSH, SourceTool)
		}
		// Defaults per-key tracking
		if sources.SourceForDefault("*", "no_input") != SourceTool {
			t.Errorf("Defaults[*][no_input] source = %q, want %q", sources.SourceForDefault("*", "no_input"), SourceTool)
		}
		if sources.SourceForDefault("*", "timeout") != SourceAgent {
			t.Errorf("Defaults[*][timeout] source = %q, want %q", sources.SourceForDefault("*", "timeout"), SourceAgent)
		}
		if sources.SourceForDefault("rm", "dry_run") != SourceAgent {
			t.Errorf("Defaults[rm][dry_run] source = %q, want %q", sources.SourceForDefault("rm", "dry_run"), SourceAgent)
		}
	})

	t.Run("env BASHY_PREFS layer precedence", func(t *testing.T) {
		agent := &Agent{
			Name: "coder",
			Prefs: ReaderPrefs{
				View:        ViewYAML,
				Help:        HelpSkill,
				BudgetBytes: 25000,
			},
		}
		envJSON := `{"view":"md","budget_bytes":15000,"defaults":{"graph *":{"depth":1}},"_custom":true}`
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			EnvJSON: envJSON,
			Agent:   agent,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.View != ViewMD || sources.View != SourceEnv {
			t.Errorf("View = %q (%s), want %q (%s)", resolved.View, sources.View, ViewMD, SourceEnv)
		}
		if resolved.BudgetBytes != 15000 || sources.BudgetBytes != SourceEnv {
			t.Errorf("BudgetBytes = %d (%s), want 15000 (%s)", resolved.BudgetBytes, sources.BudgetBytes, SourceEnv)
		}
		if resolved.Help != HelpSkill || sources.Help != SourceAgent {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpSkill, SourceAgent)
		}
		if sources.SourceForDefault("graph *", "depth") != SourceEnv {
			t.Errorf("Defaults[graph *][depth] source = %q, want %q", sources.SourceForDefault("graph *", "depth"), SourceEnv)
		}
		if sources.SourceFor("_custom") != SourceEnv {
			t.Errorf("SourceFor(_custom) = %q, want %q", sources.SourceFor("_custom"), SourceEnv)
		}
	})

	t.Run("explicit flag layer precedence", func(t *testing.T) {
		envJSON := `{"view":"md","budget_bytes":15000,"defaults":{"rm":{"dry_run":true}}}`
		explicit := ReaderPrefs{
			View:        ViewCanonical,
			BudgetBytes: 8000,
			Defaults: map[string]map[string]any{
				"rm": {"confirm": true},
			},
		}
		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			Explicit: explicit,
			EnvJSON:  envJSON,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if resolved.View != ViewCanonical || sources.View != SourceExplicit {
			t.Errorf("View = %q (%s), want %q (%s)", resolved.View, sources.View, ViewCanonical, SourceExplicit)
		}
		if resolved.BudgetBytes != 8000 || sources.BudgetBytes != SourceExplicit {
			t.Errorf("BudgetBytes = %d (%s), want 8000 (%s)", resolved.BudgetBytes, sources.BudgetBytes, SourceExplicit)
		}
		if sources.SourceForDefault("rm", "dry_run") != SourceEnv {
			t.Errorf("Defaults[rm][dry_run] source = %q, want %q", sources.SourceForDefault("rm", "dry_run"), SourceEnv)
		}
		if sources.SourceForDefault("rm", "confirm") != SourceExplicit {
			t.Errorf("Defaults[rm][confirm] source = %q, want %q", sources.SourceForDefault("rm", "confirm"), SourceExplicit)
		}
	})

	t.Run("all 6 precedence tiers together: explicit > env > agent > tool > model band > bashy default", func(t *testing.T) {
		band := 2 // sets Help="script"

		tool := &Tool{
			Name: "tool-1",
			Prefs: ReaderPrefs{
				Dialect:     DialectPWSH,
				BudgetBytes: 32000,
				Defaults: map[string]map[string]any{
					"*": {"no_input": true, "timeout": "30s"},
				},
			},
		}

		agent := &Agent{
			Name: "agent-1",
			Prefs: ReaderPrefs{
				Help: HelpSkill,
				Defaults: map[string]map[string]any{
					"*":     {"timeout": "10s"},
					"git *": {"depth": 2},
				},
			},
		}

		envJSON := `{
			"view": "yaml",
			"defaults": {
				"git *": {"depth": 1},
				"rm": {"dry_run": true}
			}
		}`

		explicit := ReaderPrefs{
			BudgetBytes: 16000,
			Defaults: map[string]map[string]any{
				"rm": {"confirm": true},
			},
		}

		resolved, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			Explicit: explicit,
			EnvJSON:  envJSON,
			Agent:    agent,
			Tool:     tool,
			Band:     band,
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// 1. View from Env ("yaml", source: env)
		if resolved.View != ViewYAML || sources.View != SourceEnv {
			t.Errorf("View = %q (%s), want %q (%s)", resolved.View, sources.View, ViewYAML, SourceEnv)
		}

		// 2. Help from Agent ("skill", source: agent) overrides tool and band
		if resolved.Help != HelpSkill || sources.Help != SourceAgent {
			t.Errorf("Help = %q (%s), want %q (%s)", resolved.Help, sources.Help, HelpSkill, SourceAgent)
		}

		// 3. Dialect from Tool ("pwsh", source: tool) overrides bashy default "bash"
		if resolved.Dialect != DialectPWSH || sources.Dialect != SourceTool {
			t.Errorf("Dialect = %q (%s), want %q (%s)", resolved.Dialect, sources.Dialect, DialectPWSH, SourceTool)
		}

		// 4. BudgetBytes from Explicit (16000, source: explicit) overrides env, agent, tool, default
		if resolved.BudgetBytes != 16000 || sources.BudgetBytes != SourceExplicit {
			t.Errorf("BudgetBytes = %d (%s), want 16000 (%s)", resolved.BudgetBytes, sources.BudgetBytes, SourceExplicit)
		}

		// 5. Defaults pattern+key fine-grained source tracking:
		//   "*" no_input from tool
		if sources.SourceForDefault("*", "no_input") != SourceTool {
			t.Errorf("Defaults[*][no_input] source = %q, want %q", sources.SourceForDefault("*", "no_input"), SourceTool)
		}
		//   "*" timeout overridden by agent
		if sources.SourceForDefault("*", "timeout") != SourceAgent {
			t.Errorf("Defaults[*][timeout] source = %q, want %q", sources.SourceForDefault("*", "timeout"), SourceAgent)
		}
		//   "git *" depth from agent overridden by env
		if sources.SourceForDefault("git *", "depth") != SourceEnv {
			t.Errorf("Defaults[git *][depth] source = %q, want %q", sources.SourceForDefault("git *", "depth"), SourceEnv)
		}
		//   "rm" dry_run from env
		if sources.SourceForDefault("rm", "dry_run") != SourceEnv {
			t.Errorf("Defaults[rm][dry_run] source = %q, want %q", sources.SourceForDefault("rm", "dry_run"), SourceEnv)
		}
		//   "rm" confirm from explicit
		if sources.SourceForDefault("rm", "confirm") != SourceExplicit {
			t.Errorf("Defaults[rm][confirm] source = %q, want %q", sources.SourceForDefault("rm", "confirm"), SourceExplicit)
		}
	})

	t.Run("invalid env JSON returns error and empty sources", func(t *testing.T) {
		_, sources, err := ResolveReaderPrefsWithSources(ReaderPrefsOptions{
			EnvJSON: "{invalid",
		})
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		if sources.View != "" {
			t.Errorf("expected empty sources on error, got View = %q", sources.View)
		}
	})

	t.Run("convenience positional and env wrappers", func(t *testing.T) {
		t.Setenv(EnvBashyPrefs, `{"view":"md"}`)
		res, src, err := ResolveReaderPrefsWithSourcesFromEnv(ReaderPrefs{}, nil, nil, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.View != ViewMD || src.View != SourceEnv {
			t.Errorf("View = %q (%s), want md (env)", res.View, src.View)
		}

		res2, src2, err := ResolveReaderPrefsWithSourcesFromArgs(ReaderPrefs{View: ViewJSON}, "", nil, nil, 0)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res2.View != ViewJSON || src2.View != SourceExplicit {
			t.Errorf("View = %q (%s), want json (explicit)", res2.View, src2.View)
		}
	})

	t.Run("clone and serialization", func(t *testing.T) {
		orig := ReaderPrefsSources{
			View:        SourceEnv,
			Help:        SourceAgent,
			Dialect:     SourceTool,
			BudgetBytes: SourceExplicit,
			Defaults: map[string]map[string]string{
				"*": {"timeout": SourceAgent},
			},
			Extra: map[string]string{
				"_custom": SourceEnv,
			},
		}

		cloned := orig.Clone()
		if cloned.View != orig.View || cloned.Defaults["*"]["timeout"] != SourceAgent || cloned.Extra["_custom"] != SourceEnv {
			t.Errorf("cloned struct does not match original: %+v", cloned)
		}
		cloned.Defaults["*"]["timeout"] = SourceExplicit
		if orig.Defaults["*"]["timeout"] != SourceAgent {
			t.Error("Clone was not a deep copy of Defaults")
		}

		data, err := json.Marshal(orig)
		if err != nil {
			t.Fatalf("json.Marshal failed: %v", err)
		}
		var decoded ReaderPrefsSources
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("json.Unmarshal failed: %v", err)
		}
		if decoded.View != orig.View || decoded.Defaults["*"]["timeout"] != SourceAgent || decoded.Extra["_custom"] != SourceEnv {
			t.Errorf("decoded does not match original: %+v", decoded)
		}
	})

	t.Run("constants alias consistency", func(t *testing.T) {
		if SourceBashyDefault != SourceDefault {
			t.Errorf("SourceBashyDefault = %q, want %q", SourceBashyDefault, SourceDefault)
		}
		if SourceBashyPrefs != SourceEnv {
			t.Errorf("SourceBashyPrefs = %q, want %q", SourceBashyPrefs, SourceEnv)
		}
		if SourceModelBand != SourceBand {
			t.Errorf("SourceModelBand = %q, want %q", SourceModelBand, SourceBand)
		}
	})
}
