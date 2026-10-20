package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/qiangli/yoke/pkg/assetring"
	"github.com/qiangli/yoke/pkg/provider"
	"github.com/qiangli/yoke/pkg/provider/scaffold"
)

// Provider packages (S472.12) load through the existing fleet kind/assetring
// mechanism, exactly like the adapter kind: one file per entry, rings
// embedded -> shared -> cloud (cached overlay) -> local, local wins, writes
// go to local. There is no parallel loader.
//
// A provider package has one of two forms (see pkg/provider/contract.go,
// which is frozen and is decoded into, never redefined here):
//
//   - declarative: data only (endpoints, templates, lifecycle mapping),
//     interpreted by the built-in engine. The loader schema-validates the
//     document without any exec. Templates are pure substitution data and
//     are never evaluated here.
//   - executable: an argv-invoked program. Argv is passed literally, never
//     through a shell. In a no-exec context (how the master links this
//     loader) an executable entry resolves as not runnable here
//     (provider.ErrExecDisabled) and is never started.
//
// builtin is the one loader-level field the contract does not carry: it
// marks a compiled-in reference entry (e.g. digitalocean) whose behavior
// ships linked into the binary as a Go adapter rather than in an
// argv-spawned program or a declarative document. A built-in is never
// dispatched to the declarative engine — the engine schema does not
// validate its document — and never spawns.

const KindInfra = "infra"
const dirInfra = "infra"

// InfraProvider is one provider package entry. Manifest is the frozen
// contract document; Builtin and Ring are loader state.
type InfraProvider struct {
	provider.Manifest `yaml:",inline"`
	// Builtin marks a compiled-in reference entry. It is loader-level only:
	// the behavior ships as the linked-in Go adapter, so the entry is
	// never dispatched to the declarative engine and never spawned.
	Builtin bool           `yaml:"builtin,omitempty" json:"builtin,omitempty" doc:"compiled-in reference entry, not a spawned program"`
	Ring    assetring.Ring `yaml:"-" json:"ring"`
}

// InfraErrKind distinguishes loader failures.
type InfraErrKind string

const (
	// InfraErrInvalid is a malformed manifest: bad shape, unknown field,
	// credential material, bad form/capabilities, or a bad declarative doc.
	InfraErrInvalid InfraErrKind = "invalid_manifest"
	// InfraErrProtocolMismatch is a well-formed manifest speaking the wrong
	// protocol version. It is distinct from invalid: the document parses,
	// but protocol must equal provider.ProtocolVersion.
	InfraErrProtocolMismatch InfraErrKind = "protocol_mismatch"
)

// InfraError is a typed loader failure.
type InfraError struct {
	Kind InfraErrKind
	Name string
	Msg  string
}

func (e *InfraError) Error() string {
	if e.Name == "" {
		return fmt.Sprintf("fleet: infra: %s: %s", e.Kind, e.Msg)
	}
	return fmt.Sprintf("fleet: infra provider %q: %s: %s", e.Name, e.Kind, e.Msg)
}

// Distinct exit codes for the two failure kinds.
const (
	ExitInfraInvalid          = 3
	ExitInfraProtocolMismatch = 4
)

// InfraExitCode maps a loader error to its exit code, falling back to the
// repo convention for anything else.
func InfraExitCode(err error) int {
	var ie *InfraError
	if errors.As(err, &ie) {
		switch ie.Kind {
		case InfraErrProtocolMismatch:
			return ExitInfraProtocolMismatch
		case InfraErrInvalid:
			return ExitInfraInvalid
		}
	}
	return assetring.ExitCode(err)
}

func infraInvalid(name, msg string) *InfraError {
	return &InfraError{Kind: InfraErrInvalid, Name: name, Msg: msg}
}

func infraMismatch(name, msg string) *InfraError {
	return &InfraError{Kind: InfraErrProtocolMismatch, Name: name, Msg: msg}
}

// infraFile is the on-disk shape. It mirrors provider.Manifest plus the
// loader-level builtin field; Declarative stays a yaml.Node so templates
// pass through byte-identical without any evaluation.
type infraFile struct {
	Name            string                `yaml:"name"`
	Kind            string                `yaml:"kind"`
	Form            provider.Form         `yaml:"form"`
	Version         string                `yaml:"version"`
	ProtocolVersion string                `yaml:"protocol_version"`
	Summary         string                `yaml:"summary"`
	Capabilities    []provider.Capability `yaml:"capabilities"`
	Argv            []string              `yaml:"argv"`
	Builtin         bool                  `yaml:"builtin"`
	Declarative     yaml.Node             `yaml:"declarative"`
}

var infraFields = map[string]bool{
	"name": true, "kind": true, "form": true, "version": true,
	"protocol_version": true, "summary": true, "capabilities": true,
	"argv": true, "builtin": true, "declarative": true,
}

// credentialKeys are manifest keys that would carry secret material. A
// provider manifest must never carry credential values: credentials are
// references resolved through pkg/secrets by the caller, never manifest
// fields. Matching is case-insensitive with _ and - stripped, so TOKEN,
// apiKey and access-token are all caught.
var credentialKeys = map[string]bool{
	"token": true, "tokens": true, "credential": true, "credentials": true,
	"secret": true, "secrets": true, "apikey": true, "password": true,
	"passwd": true, "accesstoken": true, "authtoken": true, "bearertoken": true,
	"privatekey": true, "clientsecret": true,
}

func credentialKey(k string) bool {
	n := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(k, "_", ""), "-", ""))
	return credentialKeys[n]
}

// infraCapabilities is every capability a manifest may declare: every
// lifecycle op plus the feature flags from the contract.
var infraCapabilities = func() map[provider.Capability]bool {
	m := map[provider.Capability]bool{
		provider.CapVolumes: true,
		provider.CapSpot:    true,
	}
	for _, op := range []provider.Op{
		provider.OpValidate, provider.OpCapacity, provider.OpPlan,
		provider.OpCreate, provider.OpInspect, provider.OpList,
		provider.OpStart, provider.OpStop, provider.OpDestroy,
		provider.OpReboot, provider.OpResize,
	} {
		m[provider.Capability(op)] = true
	}
	return m
}()

// ParseInfraProvider reads one provider manifest. name is the fallback
// identity when the document carries none (the file's own basename).
func ParseInfraProvider(name string, body []byte, src assetring.Source) (InfraProvider, error) {
	var n yaml.Node
	if err := yaml.Unmarshal(body, &n); err != nil {
		return InfraProvider{}, infraInvalid(name, fmt.Sprintf("invalid YAML: %v", err))
	}
	if len(n.Content) != 1 || n.Content[0].Kind != yaml.MappingNode {
		return InfraProvider{}, infraInvalid(name, "manifest must be a mapping")
	}
	keys := n.Content[0].Content
	seen := map[string]bool{}
	for i := 0; i < len(keys); i += 2 {
		k := keys[i].Value
		if seen[k] {
			return InfraProvider{}, infraInvalid(name, fmt.Sprintf("duplicate field %q", k))
		}
		seen[k] = true
		if credentialKey(k) {
			return InfraProvider{}, infraInvalid(name, fmt.Sprintf("field %q carries credential material: credentials are references resolved through pkg/secrets, never manifest fields", k))
		}
		if !infraFields[k] {
			return InfraProvider{}, infraInvalid(name, fmt.Sprintf("unknown field %q", k))
		}
	}
	var f infraFile
	d := yaml.NewDecoder(bytes.NewReader(body))
	if err := d.Decode(&f); err != nil {
		return InfraProvider{}, infraInvalid(name, fmt.Sprintf("invalid manifest: %v", err))
	}
	p := InfraProvider{
		Builtin: f.Builtin,
	}
	p.Manifest.Name = f.Name
	p.Manifest.Kind = f.Kind
	p.Manifest.Form = f.Form
	p.Manifest.Version = f.Version
	p.Manifest.ProtocolVersion = f.ProtocolVersion
	p.Manifest.Summary = f.Summary
	p.Manifest.Capabilities = f.Capabilities
	p.Manifest.Argv = f.Argv
	if f.Declarative.Kind != 0 && f.Declarative.Tag != "!!null" {
		raw, err := yamlNodeJSON(&f.Declarative)
		if err != nil {
			return InfraProvider{}, infraInvalid(name, fmt.Sprintf("invalid declarative document: %v", err))
		}
		p.Manifest.Declarative = raw
	}
	if p.Manifest.Name == "" {
		p.Manifest.Name = name
	}
	if src != nil {
		p.Ring = src.Ring()
	}
	if err := p.Validate(); err != nil {
		return InfraProvider{}, err
	}
	return p, nil
}

// yamlNodeJSON renders a YAML node as JSON without evaluating anything: the
// declarative document passes through as pure data.
func yamlNodeJSON(n *yaml.Node) (json.RawMessage, error) {
	var v any
	if err := n.Decode(&v); err != nil {
		return nil, err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

// Validate enforces the manifest contract: identity, kind, form, protocol,
// capabilities, and the per-form payload rules.
func (p InfraProvider) Validate() error {
	name := p.Manifest.Name
	if err := validName(name); err != nil {
		return infraInvalid(name, err.Error())
	}
	if p.Manifest.Kind != KindInfra {
		return infraInvalid(name, fmt.Sprintf("kind must be %q", KindInfra))
	}
	switch p.Manifest.Form {
	case provider.FormDeclarative, provider.FormExecutable:
	default:
		return infraInvalid(name, fmt.Sprintf("form must be %q or %q", provider.FormDeclarative, provider.FormExecutable))
	}
	if p.Manifest.ProtocolVersion != provider.ProtocolVersion {
		return infraMismatch(name, fmt.Sprintf("protocol_version %q != %q", p.Manifest.ProtocolVersion, provider.ProtocolVersion))
	}
	for _, c := range p.Manifest.Capabilities {
		if !infraCapabilities[c] {
			return infraInvalid(name, fmt.Sprintf("unknown capability %q", c))
		}
	}
	if p.Manifest.Form == provider.FormDeclarative {
		if len(p.Manifest.Argv) != 0 {
			return infraInvalid(name, "declarative manifest must not carry argv")
		}
		if p.Builtin {
			// A built-in is dispatched to the linked-in Go adapter,
			// never the declarative engine, so its document is not
			// engine-validated here.
			return nil
		}
		if err := validateDeclarativeDoc(p.Manifest); err != nil {
			return err
		}
		return nil
	}
	if len(p.Manifest.Declarative) != 0 {
		return infraInvalid(name, "executable plugin must not carry a declarative document")
	}
	if len(p.Manifest.Argv) == 0 {
		return infraInvalid(name, "executable plugin requires argv")
	}
	for _, tok := range p.Manifest.Argv {
		if tok == "" {
			return infraInvalid(name, "argv must not contain an empty token")
		}
	}
	return nil
}

// validateDeclarativeDoc runs the exported pkg/provider declarative
// validator — the same function the master calls at registration — so
// registration and the engine agree. It validates the schema without any
// exec and without allocating anything: templates are pure substitution
// data, never substituted or evaluated here.
func validateDeclarativeDoc(m provider.Manifest) error {
	if err := provider.ValidateDeclarative(m); err != nil {
		var perr *provider.Error
		if errors.As(err, &perr) {
			return &InfraError{Kind: InfraErrInvalid, Name: m.Name, Msg: perr.Message}
		}
		return infraInvalid(m.Name, err.Error())
	}
	return nil
}

// MarshalYAML renders the canonical bytes: the exact document a read shows
// and a write stores, with the declarative document restored (the contract
// type carries it as JSON, which YAML would otherwise drop).
func (p InfraProvider) MarshalYAML() (any, error) {
	m := map[string]any{
		"kind":             KindInfra,
		"name":             p.Manifest.Name,
		"form":             string(p.Manifest.Form),
		"protocol_version": p.Manifest.ProtocolVersion,
		"capabilities":     p.Manifest.Capabilities,
	}
	if p.Manifest.Version != "" {
		m["version"] = p.Manifest.Version
	}
	if p.Manifest.Summary != "" {
		m["summary"] = p.Manifest.Summary
	}
	if p.Builtin {
		m["builtin"] = true
	}
	if len(p.Manifest.Argv) != 0 {
		m["argv"] = p.Manifest.Argv
	}
	if len(p.Manifest.Declarative) != 0 {
		var doc any
		if err := json.Unmarshal(p.Manifest.Declarative, &doc); err != nil {
			return nil, err
		}
		m["declarative"] = doc
	}
	return m, nil
}

// InfraProviders returns every provider package, name-sorted, with the
// winning ring's origin on each row.
func (c *Catalog) InfraProviders() ([]InfraProvider, []error) {
	var errs []error
	cat := assetring.Catalog[InfraProvider]{Sources: c.sources(dirInfra), Parse: func(n string, b []byte, s assetring.Source) InfraProvider {
		errs = append(errs, sourceWarnings(s)...)
		p, err := ParseInfraProvider(n, b, s)
		if err != nil {
			errs = append(errs, fmt.Errorf("infra provider %q: %w", n, err))
			return InfraProvider{Manifest: provider.Manifest{Name: n}, Ring: s.Ring()}
		}
		return p
	}}
	rows, err := cat.Rows()
	if err != nil {
		errs = append(errs, err)
	}
	out := make([]InfraProvider, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Entry)
	}
	return out, errs
}

// InfraProvider resolves one provider package by name.
func (c *Catalog) InfraProvider(name string) (InfraProvider, bool) {
	rows, _ := c.InfraProviders()
	for _, p := range rows {
		if p.Manifest.Name == name {
			return p, true
		}
	}
	return InfraProvider{}, false
}

// SaveInfraProvider writes a provider manifest into the local store. Writes
// always land in local; lower rings are never mutated.
func (c *Catalog) SaveInfraProvider(p InfraProvider) error {
	if err := p.Validate(); err != nil {
		return err
	}
	b, err := Marshal(p)
	if err != nil {
		return err
	}
	return writeEntry(c.nounDir(dirInfra), p.Manifest.Name, b)
}

// RemoveInfraProvider deletes a provider manifest from the local store.
func (c *Catalog) RemoveInfraProvider(name string) error {
	return removeEntry(c.nounDir(dirInfra), dirInfra, name)
}

// MaterializeInfraProvider copies a lower-ring entry into the local store
// and returns its path.
func (c *Catalog) MaterializeInfraProvider(name string) (string, error) {
	p, ok := c.InfraProvider(name)
	if !ok {
		return "", fmt.Errorf("unknown infra provider")
	}
	if p.Ring != ringLocal() {
		if err := c.SaveInfraProvider(p); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirInfra), p.Manifest.Name)
}

// InfraExecFunc invokes an executable provider plugin. It receives the
// manifest argv literally — never joined, interpolated, or passed through a
// shell — and runs it directly (exec.Command style). Injected so tests can
// observe the argv or fail if it is ever called.
type InfraExecFunc func(ctx context.Context, argv []string, req provider.Request) (provider.Result, error)

// InfraResolveOptions is the loader exec policy.
type InfraResolveOptions struct {
	// AllowExec permits starting executable plugins. The master links this
	// loader with AllowExec false (no-exec context): an executable entry
	// then resolves as not runnable here and is never started.
	AllowExec bool
	// Exec runs the plugin argv. Nil means the pkg/provider executable runner.
	Exec InfraExecFunc
}

// InfraResolution is a resolved provider package.
type InfraResolution struct {
	Provider InfraProvider
	// Argv is the literal plugin argv (executable form only). It is what
	// the dispatcher spawns directly, without a shell.
	Argv []string
	// Result is the hook outcome (executable form with AllowExec only).
	Result *provider.Result
}

// ResolveInfra loads, validates, and applies the exec policy to one provider
// package. Declarative manifests validate with zero process execs — the
// hook is never consulted for them. An executable entry in a no-exec
// context fails with provider.ErrExecDisabled and is never started.
func (c *Catalog) ResolveInfra(ctx context.Context, name string, req provider.Request, opts InfraResolveOptions) (InfraResolution, error) {
	p, ok := c.InfraProvider(name)
	if !ok {
		return InfraResolution{}, fmt.Errorf("fleet: no infra provider %q", name)
	}
	if err := p.Validate(); err != nil {
		return InfraResolution{}, err
	}
	if perr := provider.CheckRequest(p.Manifest, req); perr != nil {
		return InfraResolution{}, perr
	}
	if p.Manifest.Form == provider.FormDeclarative {
		return InfraResolution{Provider: p}, nil
	}
	if !opts.AllowExec {
		return InfraResolution{}, &provider.Error{
			Code:    provider.ErrExecDisabled,
			Message: fmt.Sprintf("infra provider %q is executable and this context does not run plugins", name),
		}
	}
	argv := append([]string(nil), p.Manifest.Argv...)
	run := opts.Exec
	if run == nil {
		run = defaultInfraExec(p.Manifest)
	}
	res, err := run(ctx, argv, req)
	if err != nil {
		return InfraResolution{}, err
	}
	return InfraResolution{Provider: p, Argv: argv, Result: &res}, nil
}

// defaultInfraExec runs the plugin through the pkg/provider executable runner.
func defaultInfraExec(m provider.Manifest) InfraExecFunc {
	return func(ctx context.Context, argv []string, req provider.Request) (provider.Result, error) {
		m.Argv = argv
		a, err := provider.NewExecAdapter(m, provider.ExecOptions{})
		if err != nil {
			return provider.Result{}, err
		}
		return a.Do(ctx, req)
	}
}

func init() {
	registerKind(kindSpec{
		Name: KindInfra, Plural: dirInfra, DirEnv: "BASHY_INFRA_DIR", PathEnv: "BASHY_INFRA_PATH",
		Type:   reflect.TypeOf(InfraProvider{}),
		Lookup: holder((*Catalog).InfraProvider, func(p InfraProvider) string { return p.Manifest.Name }),
		Names:  listed((*Catalog).InfraProviders, func(p InfraProvider) string { return p.Manifest.Name }),
		Seeded: true, Mechanism: true,
		Record: newRecordSpec(typedRecord[InfraProvider]{
			Get:  (*Catalog).InfraProvider,
			Name: func(p *InfraProvider) *string { return &p.Manifest.Name },
			Ring: func(p InfraProvider) assetring.Ring { return p.Ring },
			Summary: func(cmd *cobra.Command, _ *Catalog, p InfraProvider) error {
				caps := make([]string, 0, len(p.Manifest.Capabilities))
				for _, cp := range p.Manifest.Capabilities {
					caps = append(caps, string(cp))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "name: %s\norigin: %s\nform: %s\nversion: %s\nprotocol: %s\ncapabilities: %s\n",
					p.Manifest.Name, p.Ring, p.Manifest.Form,
					p.Manifest.Version, p.Manifest.ProtocolVersion, strings.Join(caps, ", "))
				if p.Builtin {
					fmt.Fprintln(cmd.OutOrStdout(), "builtin: true")
				}
				if p.Manifest.Summary != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "summary: %s\n", p.Manifest.Summary)
				}
				return nil
			},
			Parse: func(fallback string, body []byte) ([]InfraProvider, error) {
				p, err := ParseInfraProvider(fallback, body, nil)
				if err != nil {
					return nil, err
				}
				return []InfraProvider{p}, nil
			},
			Save:     (*Catalog).SaveInfraProvider,
			FileFlag: true,
			AddDoc:   verbDoc{Use: "add FILE|-", Short: "Import a provider package manifest"},
		}),
	})
}

// newInfraInit writes a provider authoring scaffold (S472.14) into a
// target directory: a manifest (+ an executable skeleton implementing
// every standard hook for the executable form), an example config and
// deterministic fixtures.
func newInfraInit() *cobra.Command {
	var form, name, dir string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a provider authoring scaffold into a directory",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := scaffold.Init(scaffold.Options{Dir: dir, Form: provider.Form(form), Name: name}); err != nil {
				return err
			}
			display := name
			if display == "" {
				display = dir
			}
			fmt.Fprintf(cmd.OutOrStdout(), "initialized %s provider %q in %s\n", form, display, dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&form, "form", "declarative", "provider form: declarative|executable")
	cmd.Flags().StringVar(&name, "name", "", "provider name (default: target directory base)")
	cmd.Flags().StringVar(&dir, "dir", ".", "target directory")
	return cmd
}

// newInfraValidate checks a provider authoring directory without
// allocating anything: schema, capabilities, structured errors, timeout
// handling and operation identity.
func newInfraValidate() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "validate [DIR]",
		Short: "Validate a provider authoring directory without allocating",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := dir
			if len(args) == 1 {
				target = args[0]
			}
			if err := scaffold.Validate(target); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: valid\n", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&dir, "dir", ".", "provider authoring directory")
	return cmd
}

// NewInfraCmd builds the `infra` verb tree (add/list/show/rm plus the
// authoring init/validate) on the fleet kind-core write surface, like
// NewAdaptersCmd. Bashy mounts it later.
func NewInfraCmd(opts ...Option) *cobra.Command {
	var asJSON bool
	list := &cobra.Command{Use: "list", Short: "List provider packages", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		rows, errs := New(opts...).InfraProviders()
		if len(errs) > 0 {
			return errs[0]
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
		}
		for _, p := range rows {
			caps := make([]string, 0, len(p.Manifest.Capabilities))
			for _, cp := range p.Manifest.Capabilities {
				caps = append(caps, string(cp))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\n",
				p.Manifest.Name, p.Ring, p.Manifest.Form,
				p.Manifest.Version, p.Manifest.ProtocolVersion, strings.Join(caps, ","))
		}
		return nil
	}}
	list.Flags().BoolVar(&asJSON, "json", false, "JSON records")
	return newRoot(KindInfra, "Provider packages (declarative manifests and executable plugins)",
		list, newShow(KindInfra, opts), newAdd(KindInfra, opts), newRm(KindInfra, opts, (*Catalog).RemoveInfraProvider),
		newInfraInit(), newInfraValidate())
}
