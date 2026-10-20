package scaffold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/provider"
)

// Options configures provider scaffolding generation.
type Options struct {
	Dir     string
	Form    provider.Form
	Name    string
	Summary string
}

// Standard capabilities for generated providers.
var standardCaps = []provider.Capability{
	provider.Capability(provider.OpValidate),
	provider.Capability(provider.OpCapacity),
	provider.Capability(provider.OpPlan),
	provider.Capability(provider.OpCreate),
	provider.Capability(provider.OpInspect),
	provider.Capability(provider.OpList),
	provider.Capability(provider.OpStart),
	provider.Capability(provider.OpStop),
	provider.Capability(provider.OpDestroy),
}

// Init initializes a provider package scaffold in opts.Dir with the chosen form.
func Init(opts Options) error {
	if opts.Dir == "" {
		return fmt.Errorf("scaffold: directory is required")
	}
	if opts.Form == "" {
		opts.Form = provider.FormDeclarative
	}
	switch opts.Form {
	case provider.FormDeclarative, provider.FormExecutable:
	default:
		return fmt.Errorf("scaffold: unsupported form %q (must be %q or %q)", opts.Form, provider.FormDeclarative, provider.FormExecutable)
	}
	if opts.Name == "" {
		opts.Name = filepath.Base(opts.Dir)
		if opts.Name == "" || opts.Name == "." || opts.Name == "/" {
			opts.Name = "custom-provider"
		}
	}
	if opts.Summary == "" {
		opts.Summary = fmt.Sprintf("%s infra provider (%s)", opts.Name, opts.Form)
	}

	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return fmt.Errorf("scaffold: create dir: %w", err)
	}
	fixturesDir := filepath.Join(opts.Dir, "fixtures")
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		return fmt.Errorf("scaffold: create fixtures dir: %w", err)
	}

	switch opts.Form {
	case provider.FormDeclarative:
		if err := initDeclarative(opts); err != nil {
			return err
		}
	case provider.FormExecutable:
		if err := initExecutable(opts); err != nil {
			return err
		}
	}

	// Example configuration
	configExample := map[string]string{
		"provider":   opts.Name,
		"credential": "EXAMPLE_TOKEN_OR_API_KEY",
	}
	cfgBytes, _ := json.MarshalIndent(configExample, "", "  ")
	if err := os.WriteFile(filepath.Join(opts.Dir, "config.example.json"), append(cfgBytes, '\n'), 0o644); err != nil {
		return fmt.Errorf("scaffold: write config.example.json: %w", err)
	}

	// Deterministic fixtures
	accountFixture := `{"account":{"id":"acct-fixture","status":"active"}}` + "\n"
	if err := os.WriteFile(filepath.Join(fixturesDir, "account.json"), []byte(accountFixture), 0o644); err != nil {
		return fmt.Errorf("scaffold: write fixtures/account.json: %w", err)
	}

	regionsFixture := `{"regions":[{"slug":"region-1","name":"Region 1"}],"sizes":[{"slug":"small","name":"Small"}]}` + "\n"
	if err := os.WriteFile(filepath.Join(fixturesDir, "regions.json"), []byte(regionsFixture), 0o644); err != nil {
		return fmt.Errorf("scaffold: write fixtures/regions.json: %w", err)
	}

	machineFixture := `{"machine":{"id":"mach-fixture-1","name":"fixture-node","status":"running","region":"region-1","size":"small","tags":["bashy-lease:v1"],"ips":["192.0.2.1"]}}` + "\n"
	if err := os.WriteFile(filepath.Join(fixturesDir, "machine.json"), []byte(machineFixture), 0o644); err != nil {
		return fmt.Errorf("scaffold: write fixtures/machine.json: %w", err)
	}

	machinesFixture := `{"machines":[{"id":"mach-fixture-1","name":"fixture-node","status":"running","region":"region-1","size":"small","tags":["bashy-lease:v1"],"ips":["192.0.2.1"]}]}` + "\n"
	if err := os.WriteFile(filepath.Join(fixturesDir, "machines.json"), []byte(machinesFixture), 0o644); err != nil {
		return fmt.Errorf("scaffold: write fixtures/machines.json: %w", err)
	}

	return nil
}

func initDeclarative(opts Options) error {
	manifestContent := fmt.Sprintf(`kind: infra
name: %s
form: declarative
version: "1"
protocol_version: provider/v1
summary: %s
capabilities: [validate, capacity, plan, create, inspect, list, start, stop, destroy]
declarative:
  base_url: https://api.example.com
  auth:
    in: header
    name: Authorization
    value: Bearer ${credential}
  headers:
    X-Bashy-Owner: ${owner_tag}
  resource:
    id: id
    name: name
    status: status
    region: region
    size: size
    tags: tags
    public_ips: ips
    created_at: created_at
    retained: retained
  states:
    new: pending
    running: running
    stopped: stopped
    deleted: deleted
  operations:
    validate:
      method: GET
      path: /api/account
    capacity:
      method: GET
      path: /api/regions
    create:
      method: POST
      path: /api/machines
      result: machine
      body:
        name: ${name}
        region: ${spec.region}
        size: ${spec.size}
        image: ${spec.image}
        tags: ${tags}
    inspect:
      method: GET
      path: /api/machines/${resource_id}
      result: machine
    list:
      method: GET
      path: /api/machines
      result: machines
    start:
      method: POST
      path: /api/machines/${resource_id}/power
      body:
        action: start
      result: machine
    stop:
      method: POST
      path: /api/machines/${resource_id}/power
      body:
        action: stop
      result: machine
    destroy:
      method: DELETE
      path: /api/machines/${resource_id}
      result: machine
`, opts.Name, opts.Summary)

	manifestPath := filepath.Join(opts.Dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		return fmt.Errorf("scaffold: write manifest.yaml: %w", err)
	}
	return nil
}

func initExecutable(opts Options) error {
	manifestContent := fmt.Sprintf(`kind: infra
name: %s
form: executable
version: "1"
protocol_version: provider/v1
summary: %s
capabilities: [validate, capacity, plan, create, inspect, list, start, stop, destroy]
argv: ["./plugin.sh"]
`, opts.Name, opts.Summary)

	manifestPath := filepath.Join(opts.Dir, "manifest.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifestContent), 0o644); err != nil {
		return fmt.Errorf("scaffold: write manifest.yaml: %w", err)
	}

	// Executable skeleton script implementing all standard hooks
	pluginScript := `#!/bin/sh
set -e
# Executable provider skeleton generated by bashy infra init.
# Reads one JSON provider.Request from stdin, performs the hook, and outputs one JSON provider.Result.

TMP=$(mktemp)
cat > "$TMP"

OP=$(grep -o '"op":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4)

case "$OP" in
  validate)
    echo '{"protocol_version":"provider/v1","op":"validate","data":{"account":"fixture-account"}}'
    ;;
  capacity)
    echo '{"protocol_version":"provider/v1","op":"capacity","data":{"regions":["region-1"],"sizes":["small"]}}'
    ;;
  plan)
    echo '{"protocol_version":"provider/v1","op":"plan","data":{"plan":"dry-run-ok"}}'
    ;;
  create)
    echo '{"protocol_version":"provider/v1","op":"create","resource":{"id":"mach-1","name":"node","state":"running","tags":["bashy-lease:v1"]}}'
    ;;
  inspect)
    ID=$(grep -o '"resource_id":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4)
    if [ -z "$ID" ]; then ID="mach-1"; fi
    echo "{\"protocol_version\":\"provider/v1\",\"op\":\"inspect\",\"resource\":{\"id\":\"$ID\",\"name\":\"node\",\"state\":\"running\",\"tags\":[\"bashy-lease:v1\"]}}"
    ;;
  list)
    echo '{"protocol_version":"provider/v1","op":"list","resources":[{"id":"mach-1","name":"node","state":"running","tags":["bashy-lease:v1"]}]}'
    ;;
  start)
    ID=$(grep -o '"resource_id":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4)
    if [ -z "$ID" ]; then ID="mach-1"; fi
    echo "{\"protocol_version\":\"provider/v1\",\"op\":\"start\",\"resource\":{\"id\":\"$ID\",\"name\":\"node\",\"state\":\"running\",\"tags\":[\"bashy-lease:v1\"]}}"
    ;;
  stop)
    ID=$(grep -o '"resource_id":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4)
    if [ -z "$ID" ]; then ID="mach-1"; fi
    echo "{\"protocol_version\":\"provider/v1\",\"op\":\"stop\",\"resource\":{\"id\":\"$ID\",\"name\":\"node\",\"state\":\"stopped\",\"tags\":[\"bashy-lease:v1\"]},\"cost\":{\"ongoing\":true,\"note\":\"stopped node retains disks\"}}"
    ;;
  destroy)
    ID=$(grep -o '"resource_id":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4)
    if [ -z "$ID" ]; then ID="mach-1"; fi
    echo "{\"protocol_version\":\"provider/v1\",\"op\":\"destroy\",\"resource\":{\"id\":\"$ID\",\"name\":\"node\",\"state\":\"deleted\",\"tags\":[\"bashy-lease:v1\"]},\"cost\":{\"ongoing\":false}}"
    ;;
  *)
    echo "{\"protocol_version\":\"provider/v1\",\"op\":\"$OP\",\"error\":{\"code\":\"unsupported_capability\",\"message\":\"unsupported op: $OP\"}}"
    ;;
esac
rm -f "$TMP"
`
	pluginPath := filepath.Join(opts.Dir, "plugin.sh")
	if err := os.WriteFile(pluginPath, []byte(pluginScript), 0o755); err != nil {
		return fmt.Errorf("scaffold: write plugin.sh: %w", err)
	}

	// Go skeleton for Go authors
	goMain := `package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/qiangli/yoke/pkg/provider"
)

type pluginAdapter struct {
	manifest provider.Manifest
}

func (p *pluginAdapter) Manifest() provider.Manifest {
	return p.manifest
}

func (p *pluginAdapter) Do(ctx context.Context, req provider.Request) (provider.Result, error) {
	switch req.Op {
	case provider.OpValidate:
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Data: json.RawMessage(` + "`" + `{"account":"fixture"}` + "`" + `)}, nil
	case provider.OpCapacity:
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Data: json.RawMessage(` + "`" + `{"regions":["region-1"],"sizes":["small"]}` + "`" + `)}, nil
	case provider.OpPlan:
		return provider.Result{ProtocolVersion: provider.ProtocolVersion, Op: req.Op, Data: json.RawMessage(` + "`" + `{"plan":"ok"}` + "`" + `)}, nil
	case provider.OpCreate:
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion,
			Op:              req.Op,
			Resource: &provider.Resource{
				ID:    "mach-1",
				Name:  "node",
				State: provider.StateRunning,
				Tags:  req.Spec.Tags,
			},
		}, nil
	case provider.OpInspect:
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion,
			Op:              req.Op,
			Resource: &provider.Resource{
				ID:    req.ResourceID,
				Name:  "node",
				State: provider.StateRunning,
			},
		}, nil
	case provider.OpList:
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion,
			Op:              req.Op,
			Resources: []provider.Resource{
				{ID: "mach-1", Name: "node", State: provider.StateRunning},
			},
		}, nil
	case provider.OpStart:
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion,
			Op:              req.Op,
			Resource: &provider.Resource{
				ID:    req.ResourceID,
				Name:  "node",
				State: provider.StateRunning,
			},
		}, nil
	case provider.OpStop:
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion,
			Op:              req.Op,
			Resource: &provider.Resource{
				ID:    req.ResourceID,
				Name:  "node",
				State: provider.StateStopped,
			},
			Cost: &provider.CostReport{Ongoing: true, Note: "stopped node"},
		}, nil
	case provider.OpDestroy:
		return provider.Result{
			ProtocolVersion: provider.ProtocolVersion,
			Op:              req.Op,
			Resource: &provider.Resource{
				ID:    req.ResourceID,
				Name:  "node",
				State: provider.StateDeleted,
			},
			Cost: &provider.CostReport{Ongoing: false},
		}, nil
	default:
		return provider.Failure(req.Op, provider.ErrUnsupportedCapability, "unsupported op: %s", req.Op)
	}
}

func main() {
	m := provider.Manifest{
		Name:            "` + opts.Name + `",
		Kind:            provider.KindInfra,
		Form:            provider.FormExecutable,
		ProtocolVersion: provider.ProtocolVersion,
		Capabilities: []provider.Capability{
			provider.Capability(provider.OpValidate),
			provider.Capability(provider.OpCapacity),
			provider.Capability(provider.OpPlan),
			provider.Capability(provider.OpCreate),
			provider.Capability(provider.OpInspect),
			provider.Capability(provider.OpList),
			provider.Capability(provider.OpStart),
			provider.Capability(provider.OpStop),
			provider.Capability(provider.OpDestroy),
		},
		Argv: []string{"./plugin.sh"},
	}
	adapter := &pluginAdapter{manifest: m}
	if err := provider.Serve(context.Background(), adapter, os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}
}
`
	goPath := filepath.Join(opts.Dir, "main.go")
	if err := os.WriteFile(goPath, []byte(goMain), 0o644); err != nil {
		return fmt.Errorf("scaffold: write main.go: %w", err)
	}
	return nil
}

// Validate checks a provider authoring directory without allocating any resources.
func Validate(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("validate %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("validate %s: not a directory", dir)
	}

	manifestPath, err := findManifestFile(dir)
	if err != nil {
		return err
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}

	jsonBytes, err := yamlToJSON(data)
	if err != nil {
		return fmt.Errorf("manifest %s: syntax error: %w", manifestPath, err)
	}

	// Parse intermediate representation to extract declarative as RawMessage
	var raw struct {
		Name            string                `json:"name"`
		Kind            string                `json:"kind"`
		Form            provider.Form         `json:"form"`
		Version         string                `json:"version"`
		ProtocolVersion string                `json:"protocol_version"`
		Summary         string                `json:"summary"`
		Capabilities    []provider.Capability `json:"capabilities"`
		Argv            []string              `json:"argv"`
		Declarative     json.RawMessage       `json:"declarative"`
	}
	if err := json.Unmarshal(jsonBytes, &raw); err != nil {
		return fmt.Errorf("manifest %s: decode: %w", manifestPath, err)
	}

	m := provider.Manifest{
		Name:            raw.Name,
		Kind:            raw.Kind,
		Form:            raw.Form,
		Version:         raw.Version,
		ProtocolVersion: raw.ProtocolVersion,
		Summary:         raw.Summary,
		Capabilities:    raw.Capabilities,
		Argv:            raw.Argv,
		Declarative:     raw.Declarative,
	}

	return ValidateManifest(m, dir)
}

// ValidateManifest checks schema, capabilities, structured errors, timeout handling,
// and operation identity without allocating any resources.
func ValidateManifest(m provider.Manifest, baseDir string) error {
	if err := provider.ValidateManifest(m); err != nil {
		return err
	}

	for _, c := range m.Capabilities {
		op := provider.Op(c)
		if !op.Known() && c != provider.CapVolumes && c != provider.CapSpot {
			return &provider.Error{
				Code:    provider.ErrInvalidRequest,
				Message: fmt.Sprintf("manifest %s: unknown capability %q", m.Name, c),
			}
		}
	}

	switch m.Form {
	case provider.FormDeclarative:
		if len(m.Argv) != 0 {
			return &provider.Error{
				Code:    provider.ErrInvalidRequest,
				Message: fmt.Sprintf("manifest %s: declarative form must not declare argv", m.Name),
			}
		}
		if err := provider.ValidateDeclarative(m); err != nil {
			return err
		}

	case provider.FormExecutable:
		if len(m.Argv) == 0 || m.Argv[0] == "" {
			return &provider.Error{
				Code:    provider.ErrInvalidRequest,
				Message: fmt.Sprintf("manifest %s: executable form requires argv", m.Name),
			}
		}
		if len(m.Declarative) != 0 {
			return &provider.Error{
				Code:    provider.ErrInvalidRequest,
				Message: fmt.Sprintf("manifest %s: executable form must not carry declarative document", m.Name),
			}
		}
		if baseDir != "" {
			execTarget := m.Argv[0]
			if strings.HasPrefix(execTarget, "./") || strings.HasPrefix(execTarget, "../") {
				targetPath := filepath.Join(baseDir, execTarget)
				if _, err := os.Stat(targetPath); err != nil {
					return &provider.Error{
						Code:    provider.ErrInvalidRequest,
						Message: fmt.Sprintf("manifest %s: executable target %q not found in %s", m.Name, execTarget, baseDir),
					}
				}
			}
		}

	default:
		return &provider.Error{
			Code:    provider.ErrInvalidRequest,
			Message: fmt.Sprintf("manifest %s: unsupported form %q", m.Name, m.Form),
		}
	}

	return nil
}

func findManifestFile(dir string) (string, error) {
	candidates := []string{
		"manifest.yaml",
		"manifest.yml",
		"manifest.json",
		"provider.yaml",
		"provider.yml",
		"provider.json",
	}
	for _, c := range candidates {
		p := filepath.Join(dir, c)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("validate %s: no manifest file found (looked for manifest.yaml, manifest.json, provider.yaml)", dir)
}
