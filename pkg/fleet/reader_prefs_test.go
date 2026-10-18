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
