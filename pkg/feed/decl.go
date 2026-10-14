package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/itchyny/gojq"
	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/binmgr"
)

// declRegistry holds declarative specs keyed by channel name.
// The shipped openclaw.yaml is pre-registered on init; tests may add more.
var (
	declMu       sync.RWMutex
	declRegistry = map[string]*DeclSpec{}
)

func init() {
	// Best-effort load of the shipped openclaw declaration. Not fatal if absent
	// (e.g. in a test temp dir). Try cwd-relative and then package-adjacent.
	for _, p := range []string{"openclaw.yaml", filepath.Join("pkg", "feed", "openclaw.yaml")} {
		if spec, err := ParseDeclFile(p); err == nil {
			RegisterDecl(spec)
			break
		}
	}
}

// RegisterDecl registers a DeclSpec for its Name and Channel.
func RegisterDecl(spec *DeclSpec) {
	if spec == nil {
		return
	}
	declMu.Lock()
	defer declMu.Unlock()
	declRegistry[spec.Name] = spec
	if spec.Channel != "" && spec.Channel != spec.Name {
		declRegistry[spec.Channel] = spec
	}
}

// DeclForChannel returns the declarative spec for a channel, or nil.
func DeclForChannel(channel string) *DeclSpec {
	declMu.RLock()
	defer declMu.RUnlock()
	return declRegistry[channel]
}

// LoadDeclFile is a helper that reads openclaw.yaml or any decl file and
// registers it, returning the spec. Useful for CLI `feed channel add` flows.
func LoadAndRegisterDecl(path string) (*DeclSpec, error) {
	spec, err := ParseDeclFile(path)
	if err != nil {
		return nil, err
	}
	RegisterDecl(spec)
	return spec, nil
}

// DeclSpec is the declarative adapter declaration.
// It maps the symmetric envelope contract to ANY messaging CLI with no code change.
// See docs/feed-adapter-contract.md and the OpenClaw declaration for examples.
//
// Outbound: argv template and/or stdin (json or text), receipt parsing via
// exit code + JSONPath or regex for message ids.
// Inbound: JSON-lines or webhook mapping via JSONPath.
// Address mapping: channel:kind:id addresses to tool flags.
type DeclSpec struct {
	Name    string `yaml:"name" json:"name"`
	Display string `yaml:"display,omitempty" json:"display,omitempty"`
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`

	// Tool is resolved via binmgr (pinned, integrity-verified). Bin is a
	// direct path override for tests (fake CLI). When Bin is set it is used
	// verbatim; otherwise binmgr.CachedBinary / Ensure is tried before PATH.
	Tool *DeclTool `yaml:"tool,omitempty" json:"tool,omitempty"`

	Outbound *DeclOutbound `yaml:"outbound,omitempty" json:"outbound,omitempty"`
	Inbound  *DeclInbound  `yaml:"inbound,omitempty" json:"inbound,omitempty"`
	Address  *DeclAddress  `yaml:"address,omitempty" json:"address,omitempty"`
}

// DeclTool describes the pinned binary for this adapter.
type DeclTool struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
	// Bin is a direct path override (tests: path to a fake script). When set
	// it bypasses binmgr; empty means resolve via binmgr or PATH.
	Bin string `yaml:"bin,omitempty" json:"bin,omitempty"`
	// Assets, when non-empty, are binmgr assets keyed by "goos/goarch".
	// If nil/empty, binmgr is only consulted via CachedBinary.
	Assets map[string]binmgr.Asset `yaml:"assets,omitempty" json:"assets,omitempty"`
}

// DeclOutbound describes how envelopes become a CLI invocation.
type DeclOutbound struct {
	// Argv is the argv template. Each element is a Go text/template executed
	// against the envelope context ({{.Text}}, {{.ConversationID}}, etc.).
	// The first element may be the binary ({{.Bin}}) or is resolved separately.
	Argv []string `yaml:"argv,omitempty" json:"argv,omitempty"`

	Stdin   *DeclStdin   `yaml:"stdin,omitempty" json:"stdin,omitempty"`
	Receipt *DeclReceipt `yaml:"receipt,omitempty" json:"receipt,omitempty"`
}

// DeclStdin describes what, if anything, is written to the CLI's stdin.
type DeclStdin struct {
	// Mode is "json" (DeliveryRequest JSON) or "text" (rendered Text).
	// Empty means no stdin. "json" marshals ([])envelope(s) as
	// DeliveryRequest{Version, Envelopes}.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Template, when set in text mode, is rendered per envelope as the body.
	// Empty in text mode means {{.Text}}.
	Template string `yaml:"template,omitempty" json:"template,omitempty"`
}

// DeclReceipt describes how stdout is parsed for delivered message ids.
type DeclReceipt struct {
	// JSONPath is a gojq query applied to stdout JSON (e.g. "$.delivered_ids[0]").
	// JSONPath and Regex are alternatives; if both are empty the engine falls
	// back to DeliveryResult parsing and exit-code-only.
	JSONPath string `yaml:"jsonpath,omitempty" json:"jsonpath,omitempty"`
	Regex    string `yaml:"regex,omitempty" json:"regex,omitempty"`
	// ExpectExit, when false/0, means exit code !=0 is an error (default).
	// When true, non-zero exit is treated as success and receipt parsing still applies.
	ExpectExit *int `yaml:"expect_exit,omitempty" json:"expect_exit,omitempty"`
}

// DeclInbound describes how inbound JSON (NDJSON line or webhook body) maps
// via JSONPath to envelope fields.
type DeclInbound struct {
	// Mode is "jsonlines" or "webhook". Empty means jsonlines.
	Mode string `yaml:"mode,omitempty" json:"mode,omitempty"`
	// Mapping from envelope field name to JSONPath/gojq expression.
	// Field names: channel, message_id, conversation_id, sender_id,
	// sender_display, text, reply_to, received_at, is_dm, attachments.
	Mapping map[string]string `yaml:"mapping,omitempty" json:"mapping,omitempty"`
	// Webhook, when set, overrides path for webhook mode. Ignored for jsonlines.
	Webhook *DeclWebhook `yaml:"webhook,omitempty" json:"webhook,omitempty"`
}

// DeclWebhook describes webhook-specific inbound.
type DeclWebhook struct {
	Path    string            `yaml:"path,omitempty" json:"path,omitempty"`
	Mapping map[string]string `yaml:"mapping,omitempty" json:"mapping,omitempty"`
}

// DeclAddress describes how channel:kind:id addresses map to tool flags.
// The address form is "channel:kind:id" (e.g. "telegram:channel:12345").
// Each kind/id pair is rendered into flags; ChannelFlag/KindFlag/IDFlag name
// the flag that carries each component (e.g. "--channel", "--target").
type DeclAddress struct {
	ChannelFlag string `yaml:"channel_flag,omitempty" json:"channel_flag,omitempty"`
	KindFlag    string `yaml:"kind_flag,omitempty" json:"kind_flag,omitempty"`
	IDFlag      string `yaml:"id_flag,omitempty" json:"id_flag,omitempty"`
	// Pattern is an optional combined mapping, e.g. "--target {{.Kind}}:{{.ID}}"
	// When empty the three flags above are used individually.
	Pattern string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
}

// envelopeTemplateData is the template context for argv/stdin rendering.
type envelopeTemplateData struct {
	Bin            string
	Channel        string
	MessageID      string
	ID             string
	ConversationID string
	SenderID       string
	SenderDisplay  string
	Text           string
	ReplyTo        string
	IsDM           bool
	// Address components derived from ConversationID when it contains ":".
	Address     string // raw ConversationID
	AddressKind string
	AddressID   string
	// Full envelope JSON for {{.EnvelopeJSON}}
	EnvelopeJSON string
}

// ParseDecl parses a DeclSpec from YAML or JSON.
func ParseDecl(data []byte) (*DeclSpec, error) {
	var spec DeclSpec
	// Try YAML first (it also parses JSON).
	if err := yaml.Unmarshal(data, &spec); err != nil {
		// Fallback: strict JSON.
		if err2 := json.Unmarshal(data, &spec); err2 != nil {
			return nil, fmt.Errorf("feed: parse decl: %w", err)
		}
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &spec, nil
}

// ParseDeclFile reads and parses a declaration file.
func ParseDeclFile(path string) (*DeclSpec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("feed: read decl %q: %w", path, err)
	}
	spec, err := ParseDecl(data)
	if err != nil {
		return nil, fmt.Errorf("feed: %s: %w", path, err)
	}
	return spec, nil
}

// Validate checks the declaration for required fields.
func (s *DeclSpec) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("feed: decl: name is required")
	}
	if s.Tool != nil && strings.TrimSpace(s.Tool.Name) == "" && strings.TrimSpace(s.Tool.Bin) == "" {
		return errors.New("feed: decl: tool.name or tool.bin is required when tool is set")
	}
	if s.Outbound != nil {
		if s.Outbound.Stdin != nil {
			m := strings.ToLower(strings.TrimSpace(s.Outbound.Stdin.Mode))
			if m != "" && m != "json" && m != "text" {
				return fmt.Errorf("feed: decl: outbound stdin mode %q must be json or text", s.Outbound.Stdin.Mode)
			}
		}
		if s.Outbound.Receipt != nil && s.Outbound.Receipt.Regex != "" {
			if _, err := regexp.Compile(s.Outbound.Receipt.Regex); err != nil {
				return fmt.Errorf("feed: decl: bad receipt regex %q: %w", s.Outbound.Receipt.Regex, err)
			}
		}
		if s.Outbound.Receipt != nil && s.Outbound.Receipt.JSONPath != "" {
			// Normalize "$.foo" -> ".foo" for gojq.
			norm := s.Outbound.Receipt.JSONPath
			if strings.HasPrefix(norm, "$.") {
				norm = "." + strings.TrimPrefix(norm, "$.")
			} else if strings.HasPrefix(norm, "$[") {
				norm = "." + strings.TrimPrefix(norm, "$")
			} else if norm == "$" {
				norm = "."
			}
			if _, err := gojq.Parse(norm); err != nil {
				return fmt.Errorf("feed: decl: bad receipt jsonpath %q: %w", s.Outbound.Receipt.JSONPath, err)
			}
		}
	}
	if s.Inbound != nil {
		for _, m := range []map[string]string{s.Inbound.Mapping, func() map[string]string {
			if s.Inbound.Webhook != nil {
				return s.Inbound.Webhook.Mapping
			}
			return nil
		}()} {
			for _, expr := range m {
				if expr == "" {
					continue
				}
				norm := expr
				if strings.HasPrefix(norm, "$.") {
					norm = "." + strings.TrimPrefix(norm, "$.")
				} else if strings.HasPrefix(norm, "$[") {
					norm = "." + strings.TrimPrefix(norm, "$")
				} else if norm == "$" {
					norm = "."
				}
				if _, err := gojq.Parse(norm); err != nil {
					return fmt.Errorf("feed: decl: bad inbound mapping jsonpath %q: %w", expr, err)
				}
			}
		}
	}
	return nil
}

// DeclAdapter executes a DeclSpec against real CLIs.
type DeclAdapter struct {
	spec        *DeclSpec
	binResolver func(toolName string) string // override for tests
}

// NewDeclAdapter creates an adapter from a spec.
func NewDeclAdapter(spec *DeclSpec) (*DeclAdapter, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	return &DeclAdapter{spec: spec}, nil
}

// SetBinResolver overrides bin resolution (tests).
func (a *DeclAdapter) SetBinResolver(fn func(toolName string) string) {
	a.binResolver = fn
}

// ResolveBin returns the executable path for this adapter's tool.
// Priority: explicit Tool.Bin, binResolver, binmgr.CachedBinary, then name on PATH.
func (a *DeclAdapter) ResolveBin(ctx context.Context) (string, error) {
	if a.spec.Tool != nil && a.spec.Tool.Bin != "" {
		return a.spec.Tool.Bin, nil
	}
	toolName := ""
	if a.spec.Tool != nil {
		toolName = a.spec.Tool.Name
	} else if a.spec.Name != "" {
		toolName = a.spec.Name
	}
	if toolName == "" && len(a.spec.OutboundArgvBin()) > 0 {
		toolName = a.spec.OutboundArgvBin()
	}
	if a.binResolver != nil && toolName != "" {
		if p := a.binResolver(toolName); p != "" {
			return p, nil
		}
	}
	if toolName != "" {
		if cached := binmgr.CachedBinary(toolName); cached != "" {
			return cached, nil
		}
		// Try binmgr Ensure when spec carries assets/version.
		if a.spec.Tool != nil && a.spec.Tool.Version != "" && len(a.spec.Tool.Assets) > 0 {
			t := binmgr.Tool{Name: a.spec.Tool.Name, Version: a.spec.Tool.Version, Assets: a.spec.Tool.Assets}
			if p, err := binmgr.Ensure(ctx, t); err == nil {
				return p, nil
			}
		}
		if path, err := exec.LookPath(toolName); err == nil {
			return path, nil
		}
		return toolName, nil // let exec fail with clear error
	}
	return "", errors.New("feed: decl: no binary resolver")
}

// OutboundArgvBin returns the first argv element (the binary token) or "".
func (s *DeclSpec) OutboundArgvBin() string {
	if s.Outbound == nil || len(s.Outbound.Argv) == 0 {
		return ""
	}
	return s.Outbound.Argv[0]
}

// Spec returns the underlying declaration.
func (a *DeclAdapter) Spec() *DeclSpec { return a.spec }

// Send delivers envelopes via the declared CLI. It honours stdin mode,
// argv templating, and receipt parsing. Bin resolution uses binmgr (pinned,
// integrity-verified) when a Tool is declared; otherwise it falls back to
// PATH.
func (a *DeclAdapter) Send(ctx context.Context, envelopes []Envelope) ([]string, error) {
	if len(envelopes) == 0 {
		return nil, nil
	}
	for i := range envelopes {
		if envelopes[i].Version == "" {
			envelopes[i].Version = ContractVersion
		}
		if err := envelopes[i].Validate(); err != nil {
			return nil, fmt.Errorf("feed: decl envelope %d: %w", i, err)
		}
	}
	if a.spec.Outbound == nil {
		return nil, errors.New("feed: decl: outbound not configured")
	}
	binPath, err := a.ResolveBin(ctx)
	if err != nil {
		return nil, err
	}
	// Batch via stdin=json: one invocation for all envelopes.
	if a.spec.Outbound.Stdin != nil && strings.ToLower(a.spec.Outbound.Stdin.Mode) == "json" {
		return a.sendBatchJSON(ctx, binPath, envelopes)
	}
	// Otherwise per-envelope invocations.
	var ids []string
	for _, env := range envelopes {
		id, err := a.sendOne(ctx, binPath, env)
		if err != nil {
			return ids, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

func (a *DeclAdapter) sendBatchJSON(ctx context.Context, binPath string, envelopes []Envelope) ([]string, error) {
	argv, err := a.renderArgv(envelopes[0], binPath, true)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(DeliveryRequest{Version: ContractVersion, Envelopes: envelopes})
	if err != nil {
		return nil, err
	}
	// If argv is empty, the binary alone is argv.
	if len(argv) == 0 {
		argv = []string{binPath}
	}
	stdout, stderr, code, runErr := runCmd(ctx, argv[0], argv[1:], body)
	if runErr != nil && code != 0 && !a.expectNonZeroExit() {
		return nil, fmt.Errorf("feed: decl %q: %w: %s", a.spec.Name, runErr, strings.TrimSpace(stderr))
	}
	if code != 0 && !a.expectNonZeroExit() {
		return nil, fmt.Errorf("feed: decl %q: exit %d: %s", a.spec.Name, code, strings.TrimSpace(stderr+stdout))
	}
	combined := stdout
	if combined == "" {
		combined = stderr
	}
	ids, err := a.parseReceipt(combined, envelopes)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		for _, e := range envelopes {
			ids = append(ids, e.MessageID)
			if ids[len(ids)-1] == "" {
				ids[len(ids)-1] = e.ID
			}
		}
	}
	return ids, nil
}

func (a *DeclAdapter) sendOne(ctx context.Context, binPath string, env Envelope) (string, error) {
	argv, err := a.renderArgv(env, binPath, false)
	if err != nil {
		return "", err
	}
	var stdinData []byte
	if a.spec.Outbound.Stdin != nil {
		switch strings.ToLower(a.spec.Outbound.Stdin.Mode) {
		case "json":
			// Per-envelope JSON: single-envelope DeliveryRequest.
			body, _ := json.Marshal(DeliveryRequest{Version: ContractVersion, Envelopes: []Envelope{env}})
			stdinData = body
		case "text":
			tmpl := a.spec.Outbound.Stdin.Template
			if tmpl == "" {
				tmpl = "{{.Text}}"
			}
			rendered, err := renderTemplate(tmpl, templateData(env, binPath))
			if err != nil {
				return "", err
			}
			stdinData = []byte(rendered)
		}
	}
	if len(argv) == 0 {
		argv = []string{binPath}
	}
	stdout, stderr, code, runErr := runCmd(ctx, argv[0], argv[1:], stdinData)
	if runErr != nil && code != 0 && !a.expectNonZeroExit() {
		return "", fmt.Errorf("feed: decl %q: %w: %s", a.spec.Name, runErr, strings.TrimSpace(stderr))
	}
	if code != 0 && !a.expectNonZeroExit() {
		return "", fmt.Errorf("feed: decl %q: exit %d: %s", a.spec.Name, code, strings.TrimSpace(stderr+stdout))
	}
	combined := stdout
	if strings.TrimSpace(combined) == "" {
		combined = stderr
	}
	ids, err := a.parseReceipt(combined, []Envelope{env})
	if err != nil {
		return "", err
	}
	if len(ids) == 0 {
		if env.MessageID != "" {
			return env.MessageID, nil
		}
		return env.ID, nil
	}
	return ids[0], nil
}

func (a *DeclAdapter) expectNonZeroExit() bool {
	if a.spec.Outbound == nil || a.spec.Outbound.Receipt == nil || a.spec.Outbound.Receipt.ExpectExit == nil {
		return false
	}
	return *a.spec.Outbound.Receipt.ExpectExit != 0
}

// parseReceipt extracts message ids from output via JSONPath or regex.
func (a *DeclAdapter) parseReceipt(output string, envelopes []Envelope) ([]string, error) {
	out := strings.TrimSpace(output)
	if out == "" {
		return nil, nil
	}
	// Try DeliveryResult fallback first when no explicit receipt config.
	if a.spec.Outbound.Receipt == nil || (a.spec.Outbound.Receipt.JSONPath == "" && a.spec.Outbound.Receipt.Regex == "") {
		var dr DeliveryResult
		if err := json.Unmarshal([]byte(out), &dr); err == nil {
			if dr.Error != "" {
				return nil, fmt.Errorf("feed: decl %q: %s", a.spec.Name, dr.Error)
			}
			if len(dr.DeliveredIDs) > 0 {
				return dr.DeliveredIDs, nil
			}
		}
		// Whitespace-delimited ids.
		if fields := strings.Fields(out); len(fields) > 0 {
			// Heuristic: if output looks like JSON array, don't mis-parse.
			if !strings.HasPrefix(out, "{") && !strings.HasPrefix(out, "[") {
				return fields, nil
			}
		}
		return nil, nil
	}
	r := a.spec.Outbound.Receipt
	if r.JSONPath != "" {
		ids, err := evalJSONPath(out, r.JSONPath)
		if err != nil {
			return nil, fmt.Errorf("feed: decl %q: jsonpath %q: %w", a.spec.Name, r.JSONPath, err)
		}
		return ids, nil
	}
	if r.Regex != "" {
		re := regexp.MustCompile(r.Regex)
		matches := re.FindAllStringSubmatch(out, -1)
		var ids []string
		for _, m := range matches {
			if len(m) >= 2 {
				ids = append(ids, m[1])
			} else if len(m) == 1 {
				ids = append(ids, m[0])
			}
		}
		return ids, nil
	}
	return nil, nil
}

// renderArgv renders each template token in the outbound argv.
func (a *DeclAdapter) renderArgv(env Envelope, binPath string, isBatch bool) ([]string, error) {
	if a.spec.Outbound == nil || len(a.spec.Outbound.Argv) == 0 {
		return nil, nil
	}
	data := templateData(env, binPath)
	var out []string
	for _, tok := range a.spec.Outbound.Argv {
		// Resolve {{.Bin}} even when binPath differs.
		tok = strings.ReplaceAll(tok, "{{.Bin}}", binPath)
		rendered, err := renderTemplate(tok, data)
		if err != nil {
			return nil, err
		}
		// Expand only if template produced space-delimited words implicitly?
		// No: keep one token per argv entry (template stays single arg).
		out = append(out, rendered)
	}
	// First token is the binary; if it was templated to the resolved bin, keep it.
	if len(out) > 0 && out[0] == "" {
		out[0] = binPath
	}
	_ = isBatch
	return out, nil
}

func templateData(env Envelope, binPath string) envelopeTemplateData {
	kind, id := splitConversationID(env.ConversationID)
	js, _ := json.Marshal(env)
	return envelopeTemplateData{
		Bin:            binPath,
		Channel:        env.Channel,
		MessageID:      env.MessageID,
		ID:             env.ID,
		ConversationID: env.ConversationID,
		SenderID:       env.SenderID,
		SenderDisplay:  env.SenderDisplay,
		Text:           env.Text,
		ReplyTo:        env.ReplyTo,
		IsDM:           env.IsDM,
		Address:        env.ConversationID,
		AddressKind:    kind,
		AddressID:      id,
		EnvelopeJSON:   string(js),
	}
}

func splitConversationID(conv string) (kind, id string) {
	if conv == "" {
		return "", ""
	}
	if k, rest, ok := strings.Cut(conv, ":"); ok {
		// Check for channel:kind:id vs kind:id
		if rest != "" && strings.Contains(rest, ":") {
			// Take last ":" as id separator for "kind:id"
			// For "kind:id", k is kind, rest is id (may contain ":")
			// For "a:b:c", treat as kind=a:b, id=c? But spec says kind:id.
			// Simplify: split on first colon for kind, rest is id.
			return k, rest
		}
		return k, rest
	}
	return "", conv
}

func renderTemplate(text string, data envelopeTemplateData) (string, error) {
	if !strings.Contains(text, "{{") {
		return text, nil
	}
	t, err := template.New("decl").Option("missingkey=zero").Parse(text)
	if err != nil {
		return "", fmt.Errorf("feed: decl: bad template %q: %w", text, err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("feed: decl: template %q: %w", text, err)
	}
	return buf.String(), nil
}

// evalJSONPath evaluates a JSONPath/gojq query against JSON text.
// It returns the matching values as strings. The query is applied to the
// parsed JSON; an array result is flattened.
func evalJSONPath(jsonText, query string) ([]string, error) {
	var data any
	if err := json.Unmarshal([]byte(jsonText), &data); err != nil {
		return nil, fmt.Errorf("not json: %w", err)
	}
	// Normalize "$." prefix to "." for gojq.
	norm := strings.TrimSpace(query)
	if norm == "$" {
		norm = "."
	} else if strings.HasPrefix(norm, "$.") {
		norm = "." + strings.TrimPrefix(norm, "$.")
	} else if strings.HasPrefix(norm, "$[") {
		norm = "." + strings.TrimPrefix(norm, "$")
	}
	q, err := gojq.Parse(norm)
	if err != nil {
		return nil, err
	}
	iter := q.Run(data)
	var out []string
	for {
		v, ok := iter.Next()
		if !ok {
			break
		}
		if errVal, ok := v.(error); ok {
			return nil, errVal
		}
		if v == nil {
			continue
		}
		switch vals := v.(type) {
		case []any:
			for _, item := range vals {
				out = append(out, fmt.Sprint(item))
			}
		default:
			out = append(out, fmt.Sprint(v))
		}
	}
	return out, nil
}

// runCmd runs argv[0] with args, feeding stdinData if non-nil, and returns
// stdout, stderr, exit code and error. Missing binary produces an error.
func runCmd(ctx context.Context, bin string, args []string, stdinData []byte) (string, string, int, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	if stdinData != nil {
		cmd.Stdin = bytes.NewReader(stdinData)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			// Binary not found or other start failure.
			return stdout.String(), stderr.String(), -1, err
		}
	}
	return stdout.String(), stderr.String(), code, nil
}

// AddressFlags renders the DeclAddress mapping for envelope's address into
// extra argv flags. For example, address "channel:123" with
// KindFlag="--channel" IDFlag="--target" produces ["--channel","channel","--target","123"].
func (s *DeclSpec) AddressFlags(env Envelope) []string {
	if s.Address == nil {
		return nil
	}
	kind, id := splitConversationID(env.ConversationID)
	var flags []string
	if s.Address.Pattern != "" {
		rendered, _ := renderTemplate(s.Address.Pattern, templateData(env, ""))
		// Pattern expands to shell-like tokens; split on whitespace as simple.
		for _, f := range strings.Fields(rendered) {
			flags = append(flags, f)
		}
		return flags
	}
	_ = kind
	_ = id
	data := templateData(env, "")
	// Helpers to avoid emitting empty flag values.
	if s.Address.ChannelFlag != "" && env.Channel != "" {
		flags = append(flags, s.Address.ChannelFlag, env.Channel)
	}
	if s.Address.KindFlag != "" && kind != "" {
		// Only emit when kind is non-empty (conversation contained ":")
		flags = append(flags, s.Address.KindFlag, kind)
	}
	if s.Address.IDFlag != "" {
		val := id
		if val == "" {
			val = env.ConversationID
		}
		// Render IDFlag's value as template if it contains "{{"
		if strings.Contains(s.Address.IDFlag, "{{") {
			rendered, _ := renderTemplate(s.Address.IDFlag, data)
			flags = append(flags, rendered)
		} else {
			flags = append(flags, s.Address.IDFlag, val)
		}
	}
	return flags
}

// ---- inbound mapping ----

// InboundEnvelopeFromJSON maps an inbound JSON document (one line or webhook
// body) via the declaration's Mapping/JSONPaths to an Envelope.
func (a *DeclAdapter) InboundEnvelopeFromJSON(raw []byte) (Envelope, error) {
	if a.spec.Inbound == nil {
		return Envelope{}, errors.New("feed: decl: inbound not configured")
	}
	mapping := a.spec.Inbound.Mapping
	if a.spec.Inbound.Mode == "webhook" && a.spec.Inbound.Webhook != nil && len(a.spec.Inbound.Webhook.Mapping) > 0 {
		mapping = a.spec.Inbound.Webhook.Mapping
	}
	if len(mapping) == 0 {
		// No mapping: try direct envelope JSON.
		var env Envelope
		if err := json.Unmarshal(raw, &env); err == nil {
			if err2 := env.Validate(); err2 == nil {
				return env, nil
			}
		}
		return Envelope{}, fmt.Errorf("feed: decl %q: no inbound mapping and body is not an envelope: %s", a.spec.Name, string(raw))
	}
	var data any
	if err := json.Unmarshal(raw, &data); err != nil {
		return Envelope{}, fmt.Errorf("feed: decl: inbound json: %w", err)
	}
	env := Envelope{Version: ContractVersion}
	for field, expr := range mapping {
		val, err := evalJSONPathOnData(data, expr)
		if err != nil || val == "" {
			continue
		}
		switch strings.ToLower(field) {
		case "channel":
			env.Channel = val
		case "message_id", "messageid":
			env.MessageID = val
		case "conversation_id", "conversationid":
			env.ConversationID = val
		case "sender_id", "senderid":
			env.SenderID = val
		case "sender_display", "senderdisplay":
			env.SenderDisplay = val
		case "text", "body":
			env.Text = val
		case "reply_to", "replyto":
			env.ReplyTo = val
		case "received_at", "receivedat", "timestamp":
			if t, err := time.Parse(time.RFC3339, val); err == nil {
				env.ReceivedAt = t
			} else if t, err := time.Parse(time.RFC3339Nano, val); err == nil {
				env.ReceivedAt = t
			} else {
				// Try Unix millis/seconds as string.
				if ms, err := fmt.Sscanf(val, "%d", new(int64)); err == nil && ms == 1 {
					// Handled above via direct parse attempt
				}
				// Leave zero; caller fills.
				env.ReceivedAt = time.Now().UTC()
			}
		case "is_dm", "isdm":
			env.IsDM = val == "true" || val == "1"
		}
	}
	// Defaults: channel from spec, sender fallback, received_at now.
	if env.Channel == "" && a.spec.Channel != "" {
		env.Channel = a.spec.Channel
	}
	if env.Channel == "" {
		env.Channel = a.spec.Name
	}
	if env.SenderID == "" {
		env.SenderID = "unknown"
	}
	if env.MessageID == "" {
		env.MessageID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	if env.ConversationID == "" {
		env.ConversationID = "default"
	}
	if env.ReceivedAt.IsZero() {
		env.ReceivedAt = time.Now().UTC()
	}
	if err := env.Validate(); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

func evalJSONPathOnData(data any, expr string) (string, error) {
	norm := strings.TrimSpace(expr)
	if norm == "$" {
		norm = "."
	} else if strings.HasPrefix(norm, "$.") {
		norm = "." + strings.TrimPrefix(norm, "$.")
	} else if strings.HasPrefix(norm, "$[") {
		norm = "." + strings.TrimPrefix(norm, "$")
	}
	q, err := gojq.Parse(norm)
	if err != nil {
		return "", err
	}
	iter := q.Run(data)
	v, ok := iter.Next()
	if !ok {
		return "", nil
	}
	if errVal, ok := v.(error); ok {
		return "", errVal
	}
	if v == nil {
		return "", nil
	}
	return fmt.Sprint(v), nil
}

// ProcessInboundNDJSON reads NDJSON lines from r, maps each via the declaration
// to an envelope, and calls handler for each valid one (with dedupe).
func (a *DeclAdapter) ProcessInboundNDJSON(ctx context.Context, r *bytes.Reader, handler InboundHandler, pairing PairingChecker) error {
	// Accept any io.Reader by wrapping; keep *bytes.Reader fast-path.
	return a.ProcessInboundReader(ctx, r, handler, pairing)
}

// ProcessInboundReader reads NDJSON lines from r, maps each via the declaration
// to an envelope, and calls handler for each valid one (with dedupe).
func (a *DeclAdapter) ProcessInboundReader(ctx context.Context, r interface{ Read([]byte) (int, error) }, handler InboundHandler, pairing PairingChecker) error {
	if pairing == nil {
		pairing = AllowAllPairing{}
	}
	seen := map[string]bool{}
	data, err := readAllFrom(r)
	if err != nil {
		return err
	}
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		env, err := a.InboundEnvelopeFromJSON(line)
		if err != nil {
			continue
		}
		if !pairing.IsPaired(env.SenderID) {
			continue
		}
		key := env.OriginKey()
		if seen[key] {
			continue
		}
		seen[key] = true
		if handler != nil {
			if err := handler(ctx, env); err != nil {
				continue
			}
		}
	}
	return nil
}

func readAllFrom(r interface{ Read([]byte) (int, error) }) ([]byte, error) {
	var buf bytes.Buffer
	tmp := make([]byte, 4096)
	for {
		n, err := r.Read(tmp)
		if n > 0 {
			buf.Write(tmp[:n])
		}
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return nil, err
		}
		if n == 0 {
			break
		}
	}
	return buf.Bytes(), nil
}
