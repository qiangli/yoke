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

	// Executable skeleton script implementing all standard hooks. It
	// keeps its fixture state in $PLUGIN_STATE_DIR (default
	// ./.plugin-state): one state file per resource, a key file mapping
	// idempotency keys to resource IDs, and a counter for fresh IDs. A
	// create replayed under its key returns the same resource instead of
	// allocating another; unknown IDs answer not_found; stop reports the
	// stopped state with ongoing cost and never deletes.
	pluginScript := `#!/bin/sh
set -e
# Executable provider skeleton generated by bashy infra init.
# Reads one JSON provider.Request from stdin, performs the hook, and outputs one JSON provider.Result.

STATE_DIR=${PLUGIN_STATE_DIR:-$PWD/.plugin-state}
mkdir -p "$STATE_DIR"

TMP=$(mktemp)
cat > "$TMP"

OP=$(grep -o '"op":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4 || true)
KEY=$(grep -o '"idempotency_key":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4 || true)
ID=$(grep -o '"resource_id":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4 || true)

known() { [ -n "$1" ] && [ -f "$STATE_DIR/state-$1" ]; }
res_state() { cat "$STATE_DIR/state-$1"; }
res_name() { cat "$STATE_DIR/name-$1"; }

emit() {
  # emit <op> <id> <state> [<cost-json>]
  echo "{\"protocol_version\":\"provider/v1\",\"op\":\"$1\",\"resource\":{\"id\":\"$2\",\"name\":\"$(res_name "$2")\",\"state\":\"$3\",\"tags\":[\"bashy-lease:v1\"]}$4}"
}

fail() {
  # fail <op> <code> <message>
  echo "{\"protocol_version\":\"provider/v1\",\"op\":\"$1\",\"error\":{\"code\":\"$2\",\"message\":\"$3\"}}"
}

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
    if [ -n "$KEY" ] && [ -f "$STATE_DIR/keys" ]; then
      OLD=$(grep -F -m1 "$KEY " "$STATE_DIR/keys" | cut -d' ' -f2 || true)
      if [ -n "$OLD" ] && known "$OLD"; then
        emit create "$OLD" "$(res_state "$OLD")"
        rm -f "$TMP"
        exit 0
      fi
    fi
    N=0
    if [ -f "$STATE_DIR/counter" ]; then N=$(cat "$STATE_DIR/counter"); fi
    N=$((N + 1))
    echo "$N" > "$STATE_DIR/counter"
    ID="mach-$N"
    NAME=$(grep -o '"name":"[^"]*"' "$TMP" | head -1 | cut -d'"' -f4 || true)
    if [ -z "$NAME" ]; then NAME="node-$N"; fi
    echo "running" > "$STATE_DIR/state-$ID"
    echo "$NAME" > "$STATE_DIR/name-$ID"
    if [ -n "$KEY" ]; then echo "$KEY $ID" >> "$STATE_DIR/keys"; fi
    emit create "$ID" running
    ;;
  inspect)
    if ! known "$ID"; then
      fail inspect not_found "unknown resource: $ID"
    else
      emit inspect "$ID" "$(res_state "$ID")"
    fi
    ;;
  list)
    ITEMS=""
    for s in "$STATE_DIR"/state-*; do
      [ -e "$s" ] || break
      RID=$(basename "$s" | sed 's/^state-//')
      if [ -n "$ITEMS" ]; then ITEMS="$ITEMS,"; fi
      ITEMS="$ITEMS{\"id\":\"$RID\",\"name\":\"$(res_name "$RID")\",\"state\":\"$(res_state "$RID")\",\"tags\":[\"bashy-lease:v1\"]}"
    done
    echo "{\"protocol_version\":\"provider/v1\",\"op\":\"list\",\"resources\":[$ITEMS]}"
    ;;
  start)
    if ! known "$ID"; then
      fail start not_found "unknown resource: $ID"
    else
      echo "running" > "$STATE_DIR/state-$ID"
      emit start "$ID" running
    fi
    ;;
  stop)
    if ! known "$ID"; then
      fail stop not_found "unknown resource: $ID"
    else
      echo "stopped" > "$STATE_DIR/state-$ID"
      emit stop "$ID" stopped ',"cost":{"ongoing":true,"note":"stopped node retains disks"}'
    fi
    ;;
  destroy)
    if ! known "$ID"; then
      fail destroy not_found "unknown resource: $ID"
    else
      rm -f "$STATE_DIR/state-$ID" "$STATE_DIR/name-$ID"
      echo "{\"protocol_version\":\"provider/v1\",\"op\":\"destroy\",\"resource\":{\"id\":\"$ID\",\"name\":\"node\",\"state\":\"deleted\",\"tags\":[\"bashy-lease:v1\"]},\"cost\":{\"ongoing\":false}}"
    fi
    ;;
  *)
    fail "$OP" unsupported_capability "unsupported op: $OP"
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
	m, err := Load(dir)
	if err != nil {
		return err
	}

	return ValidateManifest(m, dir)
}

// Load reads the manifest in dir without allocating any resources. It
// reports the manifest the validator and the dispatcher would see.
func Load(dir string) (provider.Manifest, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return provider.Manifest{}, fmt.Errorf("load %s: %w", dir, err)
	}
	if !info.IsDir() {
		return provider.Manifest{}, fmt.Errorf("load %s: not a directory", dir)
	}

	manifestPath, err := findManifestFile(dir)
	if err != nil {
		return provider.Manifest{}, err
	}

	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return provider.Manifest{}, fmt.Errorf("read manifest %s: %w", manifestPath, err)
	}

	jsonBytes, err := yamlToJSON(data)
	if err != nil {
		return provider.Manifest{}, fmt.Errorf("manifest %s: syntax error: %w", manifestPath, err)
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
		return provider.Manifest{}, fmt.Errorf("manifest %s: decode: %w", manifestPath, err)
	}

	return provider.Manifest{
		Name:            raw.Name,
		Kind:            raw.Kind,
		Form:            raw.Form,
		Version:         raw.Version,
		ProtocolVersion: raw.ProtocolVersion,
		Summary:         raw.Summary,
		Capabilities:    raw.Capabilities,
		Argv:            raw.Argv,
		Declarative:     raw.Declarative,
	}, nil
}

// ValidateManifest checks schema, capabilities, structured errors, timeout handling,
// and operation identity without allocating any resources.
func ValidateManifest(m provider.Manifest, baseDir string) error {
	if err := provider.ValidateManifest(m); err != nil {
		return err
	}

	seen := map[provider.Capability]bool{}
	for _, c := range m.Capabilities {
		if seen[c] {
			return &provider.Error{
				Code:    provider.ErrInvalidRequest,
				Message: fmt.Sprintf("manifest %s: duplicate capability %q: one capability names one operation", m.Name, c),
			}
		}
		seen[c] = true
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
