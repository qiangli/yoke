package fleet

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/assetring"
)

// Sprint 418 S15 (story d1d045dd536f): service units in the registry rings.

var builtinUnits = []string{"actrunner", "apps", "events", "files", "headlamp", "kopia", "lan-inference", "llm", "loom", "meet",
	"ollama", "ollama-pool", "otel", "podman", "sandbox", "schedule", "sdlc", "seaweedfs", "shard", "warm", "ycode", "zot"}

func serviceOpts(root string) []Option {
	return []Option{WithRoot(root), WithoutCloudOverlay()}
}

func svcRun(t *testing.T, opts []Option, stdin string, args ...string) (string, error) {
	t.Helper()
	cmd := NewServiceCmd(opts...)
	if stdin != "" {
		cmd.SetIn(strings.NewReader(stdin))
	}
	return runCmd(t, cmd, args...)
}

func mustSvc(t *testing.T, opts []Option, stdin string, args ...string) string {
	t.Helper()
	out, err := svcRun(t, opts, stdin, args...)
	if err != nil {
		t.Fatalf("service %v: %v\n%s", args, err, out)
	}
	return out
}

func TestBuiltinServiceSeedsAreEmbeddedAndValid(t *testing.T) {
	t.Setenv(SeedsEnv, "off") // units are mechanism: they survive seeds-off
	root := t.TempDir()
	units, errs := New(serviceOpts(root)...).Services()
	if len(errs) != 0 {
		t.Fatalf("parse errors: %v", errs)
	}
	var names []string
	for _, s := range units {
		names = append(names, s.Name)
		if s.Ring != assetring.RingEmbedded {
			t.Errorf("%s ring = %v, want embedded", s.Name, s.Ring)
		}
		if err := s.Validate(); err != nil {
			t.Errorf("seed %s: %v", s.Name, err)
		}
	}
	if !reflect.DeepEqual(names, builtinUnits) {
		t.Fatalf("seeded units = %v, want %v", names, builtinUnits)
	}
	ports := map[string]int{
		"apps": 22749, "meet": 8637, "llm": 24556, "loom": 31880,
		"zot": 5000, "seaweedfs": 8333, "kopia": 51515,
	}
	cat := New(serviceOpts(root)...)
	for name, want := range ports {
		s, _ := cat.Service(name)
		if p, _ := s.PrimaryPort(); p.Port != want {
			t.Errorf("%s primary port = %d, want %d", name, p.Port, want)
		}
	}
	if ev, _ := cat.Service("events"); ev.ProviderName() != ServiceProviderInternal || ev.Owner != "meet" {
		t.Errorf("events = %+v, want internal capability hosted by meet", ev)
	}
	if meet, _ := cat.Service("meet"); !reflect.DeepEqual(meet.Exec, []string{"bashy", "meet", "serve", "--port", "{port}", "--bind", "{bind}"}) {
		t.Errorf("meet exec = %v, want foreground serve", meet.Exec)
	}
	if sdlc, _ := cat.Service("sdlc"); !reflect.DeepEqual(sdlc.Exec, []string{"bashy", "sdlc", "serve", "--interval", "{settings.interval}"}) {
		t.Errorf("sdlc exec = %v, want foreground serve", sdlc.Exec)
	}
	if zot, _ := cat.Service("zot"); !reflect.DeepEqual(zot.Exec, []string{"bashy", "zot", "serve", "--port", "{port}", "--addr", "{bind}", "--data={settings.data_dir}", "--zot-version", "{settings.version}"}) {
		t.Errorf("zot exec = %v, want foreground serve", zot.Exec)
	}
	if sw, _ := cat.Service("seaweedfs"); !reflect.DeepEqual(sw.Exec, []string{"bashy", "seaweedfs", "serve", "--port", "{port}", "--addr", "{bind}", "--data={settings.data_dir}", "--version", "{settings.version}"}) {
		t.Errorf("seaweedfs exec = %v, want foreground serve", sw.Exec)
	}
	if kopia, _ := cat.Service("kopia"); !reflect.DeepEqual(kopia.Exec, []string{"bashy", "kopia", "serve", "--port", "{port}", "--addr", "{bind}", "--data={settings.data_dir}", "--version", "{settings.version}"}) {
		t.Errorf("kopia exec = %v, want foreground serve", kopia.Exec)
	}
	// Seeds are never copied: nothing appears in the local store.
	if _, err := os.Stat(filepath.Join(root, dirServices)); err == nil {
		t.Fatal("reading seeds wrote the local store")
	}
}

const customUnit = `name: openclaw
kind: service
description: OpenClaw gateway
exec: ['{download}', gateway, --port, '{port}']
download:
  github: openclaw/openclaw
  version: v1.2.3
  sha256:
    linux/amd64: 0000000000000000000000000000000000000000000000000000000000000000
env:
  LOG_LEVEL: info
secrets:
  - env: OPENCLAW_TOKEN
    ref: openclaw-token
ports:
  - name: http
    port: 18789
health:
  http: /health
  interval: 10s
requires: [llm]
expose:
  app: {name: openclaw, require_login: true}
  mesh:
    - name: openclaw
enabled: true
`

// Acceptance: show --yaml piped to add -f - is the identity, for a built-in
// and for a custom unit.
func TestServiceShowYAMLAddFileIsIdentity(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	mustSvc(t, opts, customUnit, "add", "-f", "-")
	for _, name := range []string{"meet", "openclaw"} {
		before := mustSvc(t, opts, "", "show", name, "--yaml")
		mustSvc(t, opts, before, "add", "-f", "-")
		after := mustSvc(t, opts, "", "show", name, "--yaml")
		if before != after {
			t.Fatalf("%s: show|add -f - is not identity:\n--- before\n%s--- after\n%s", name, before, after)
		}
	}
	// The built-in round trip is a full local copy; rm restores the seed.
	mustSvc(t, opts, "", "rm", "meet")
	if s, _ := New(opts...).Service("meet"); s.Ring != assetring.RingEmbedded {
		t.Fatalf("after rm, meet ring = %v, want embedded", s.Ring)
	}
}

func TestServiceVerifyVerbUsesUnitWriteValidation(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	mustSvc(t, opts, "name: verify-unit\nkind: service\nexec: [/bin/true]\nenabled: false\n", "add", "-f", "-")
	out := mustSvc(t, opts, "", "verify", "verify-unit")
	if !strings.Contains(out, "ok   verify-unit") {
		t.Fatalf("verify output = %q", out)
	}
}

// Acceptance: a conflicting port is refused with the holder named, at add,
// set and enable; a disabled alternative may declare the same port.
func TestServicePortConflictNamesHolder(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	mustSvc(t, opts, "", "enable", "meet")
	clash := "name: chat2\nkind: service\nexec: [chat2, --port, '{port}']\nports:\n  - port: 8637\nenabled: true\n"
	out, err := svcRun(t, opts, clash, "add", "-f", "-")
	if err == nil || !strings.Contains(err.Error(), `held by service "meet"`) {
		t.Fatalf("add over meet's port: err=%v out=%s", err, out)
	}
	// Disabled, it is an alternative and is accepted…
	mustSvc(t, opts, strings.Replace(clash, "enabled: true", "enabled: false", 1), "add", "-f", "-")
	// …but enabling it while meet holds the port is refused.
	if _, err := svcRun(t, opts, "", "enable", "chat2"); err == nil || !strings.Contains(err.Error(), `"meet"`) {
		t.Fatalf("enable over meet's port: err=%v", err)
	}
	// A LAN bind overlaps loopback; a different port does not conflict.
	if _, err := svcRun(t, opts, "", "set", "chat2", "--set", "ports.0.bind=lan", "--set", "enabled=true"); err == nil {
		t.Fatal("lan bind on meet's port accepted")
	}
	mustSvc(t, opts, "", "set", "chat2", "--set", "ports.0.port=8638")
	mustSvc(t, opts, "", "enable", "chat2")
	// Once meet is disabled the port is free again.
	mustSvc(t, opts, "", "disable", "meet")
	mustSvc(t, opts, "", "set", "chat2", "--set", "ports.0.port=8637")
}

// Rod not fish: a seeded built-in, an internal capability and a fresh
// custom unit go through the same verbs, and an operator override is a
// sparse overlay the generic machinery honours.
func TestServiceSameVerbsForBuiltinInternalAndCustom(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	mustSvc(t, opts, customUnit, "add", "-f", "-")
	for _, name := range []string{"apps", "events", "openclaw"} {
		mustSvc(t, opts, "", "disable", name)
		mustSvc(t, opts, "", "enable", name)
		mustSvc(t, opts, "", "set", name, "--set", "settings.note=x")
	}
	// Override: move meet's port. The overlay is sparse and the argv follows.
	mustSvc(t, opts, "", "set", "meet", "--set", "ports.0.port=8700")
	ov := matrixOverlay(t, root, dirServices, "meet")
	if strings.Contains(ov, "exec:") || !strings.Contains(ov, "8700") {
		t.Fatalf("meet override is not a sparse port overlay:\n%s", ov)
	}
	meet, _ := New(opts...).Service("meet")
	argv, err := meet.ExpandExec("")
	if err != nil || strings.Join(argv, " ") != "bashy meet serve --port 8700 --bind 127.0.0.1" {
		t.Fatalf("meet argv = %q, %v", argv, err)
	}

	var env struct {
		Kind  string       `json:"kind"`
		Items []serviceRow `json:"items"`
	}
	if err := json.Unmarshal([]byte(mustSvc(t, opts, "", "list", "--json")), &env); err != nil {
		t.Fatal(err)
	}
	got := map[string]serviceRow{}
	for _, r := range env.Items {
		got[r.Name] = r
	}
	if env.Kind != KindService || len(got) != len(builtinUnits)+1 {
		t.Fatalf("list --json = %+v", env)
	}
	for _, name := range []string{"apps", "events", "openclaw"} {
		if !got[name].Enabled || got[name].Ring != "local" {
			t.Errorf("%s row = %+v, want enabled local", name, got[name])
		}
	}
	if r := got["events"]; r.Provider != ServiceProviderInternal || !reflect.DeepEqual(r.Requires, []string{"meet"}) {
		t.Errorf("events row = %+v, want internal requiring its owner", r)
	}
	if r := got["openclaw"]; r.App != "openclaw" || !reflect.DeepEqual(r.Mesh, []string{"openclaw"}) {
		t.Errorf("openclaw exposure = %+v", r)
	}
}

func TestServiceValidateRefusesMalformedUnits(t *testing.T) {
	cases := map[string]string{
		"no exec":            "name: a\n",
		"internal with exec": "name: a\nprovider: internal\ncapability: x\nowner: outpost\nexec: [x]\n",
		"internal no owner":  "name: a\nprovider: internal\ncapability: x\n",
		"bad provider":       "name: a\nprovider: docker\nexec: [x]\n",
		"port range":         "name: a\nexec: [x]\nports: [{port: 70000}]\n",
		"two probes":         "name: a\nexec: [x]\nports: [{port: 1}]\nhealth: {http: /h, tcp: true}\n",
		"bad duration":       "name: a\nexec: [x]\nrestart: {backoff: {min: soon}}\n",
		"inline secret":      "name: a\nexec: [x]\nsecrets: [{env: TOKEN}]\n",
		"unknown listener":   "name: a\nexec: [x]\nports: [{name: http, port: 1}]\nexpose: {mesh: [{name: m, port: grpc}]}\n",
		"unset placeholder":  "name: a\nexec: [x, '{settings.missing}']\n",
		"download unset":     "name: a\nexec: ['{download}']\n",
		"requires self":      "name: a\nexec: [x]\nrequires: [a]\n",
		"unknown field":      "name: a\nexec: [x]\nbashy_services: []\n",
	}
	for label, body := range cases {
		s, err := ParseService("a", []byte(body), nil)
		if err == nil {
			err = s.Validate()
		}
		if err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestServiceExpandExec(t *testing.T) {
	s := Service{Name: "z", Exec: []string{"{download}", "--http", "{bind.http}:{port.http}", "--s3", "{port.s3}", "--dir={settings.dir}"},
		Download: &CommandDownload{GitHub: "o/r", Version: "v1", SHA256: map[string]string{"linux/amd64": "00"}},
		Ports:    []ServicePort{{Name: "http", Port: 5000, Bind: "lan"}, {Name: "s3", Port: 8333}},
		Settings: map[string]string{"dir": "/data"}}
	argv, err := s.ExpandExec("/cache/zot")
	if err != nil {
		t.Fatal(err)
	}
	want := "/cache/zot --http lan:5000 --s3 8333 --dir=/data"
	if strings.Join(argv, " ") != want {
		t.Fatalf("argv = %q, want %q", strings.Join(argv, " "), want)
	}
	if _, err := s.ExpandExec(""); err == nil {
		t.Fatal("unprovisioned {download} expanded")
	}
}

func TestExternalServiceSeeds_ParseValidateExpandExec(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)

	type testCase struct {
		name        string
		port        int
		mesh        string
		healthHTTP  string
		startPeriod string
		defaultArgv string
		customData  string
		customArgv  string
	}

	cases := []testCase{
		{
			name:        "zot",
			port:        5000,
			mesh:        "registry",
			healthHTTP:  "/v2/",
			startPeriod: "30s",
			defaultArgv: "bashy zot serve --port 5000 --addr 127.0.0.1 --data= --zot-version bashy-v2.1.22-2",
			customData:  "/var/lib/zot",
			customArgv:  "bashy zot serve --port 5001 --addr 0.0.0.0 --data=/var/lib/zot --zot-version bashy-v2.1.22-2",
		},
		{
			name:        "seaweedfs",
			port:        8333,
			mesh:        "s3",
			healthHTTP:  "/",
			startPeriod: "45s",
			defaultArgv: "bashy seaweedfs serve --port 8333 --addr 127.0.0.1 --data= --version bashy-4.48-1",
			customData:  "/var/lib/seaweedfs",
			customArgv:  "bashy seaweedfs serve --port 8334 --addr 0.0.0.0 --data=/var/lib/seaweedfs --version bashy-4.48-1",
		},
		{
			name:        "kopia",
			port:        51515,
			mesh:        "backup",
			healthHTTP:  "/",
			startPeriod: "30s",
			defaultArgv: "bashy kopia serve --port 51515 --addr 127.0.0.1 --data= --version bashy-v0.23.1-3",
			customData:  "/var/lib/kopia",
			customArgv:  "bashy kopia serve --port 51516 --addr 0.0.0.0 --data=/var/lib/kopia --version bashy-v0.23.1-3",
		},
	}

	for _, tc := range cases {
		s, ok := cat.Service(tc.name)
		if !ok {
			t.Fatalf("seed %q not found", tc.name)
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("%s validate: %v", tc.name, err)
		}
		if s.ProviderName() != ServiceProviderProcess {
			t.Errorf("%s provider = %q, want process", tc.name, s.ProviderName())
		}
		p, ok := s.PrimaryPort()
		if !ok || p.Port != tc.port || (p.Bind != "" && p.Bind != "loopback") {
			t.Errorf("%s primary port = %+v, want %d loopback", tc.name, p, tc.port)
		}
		if s.Health.HTTP != tc.healthHTTP || s.Health.StartPeriod != tc.startPeriod {
			t.Errorf("%s health = %+v, want http=%q start_period=%q", tc.name, s.Health, tc.healthHTTP, tc.startPeriod)
		}
		if len(s.Expose.Mesh) != 1 || s.Expose.Mesh[0].Name != tc.mesh {
			t.Errorf("%s mesh = %+v, want [%s]", tc.name, s.Expose.Mesh, tc.mesh)
		}
		if s.Upgrade != "restart" {
			t.Errorf("%s upgrade = %q, want restart", tc.name, s.Upgrade)
		}
		if s.Enabled {
			t.Errorf("%s enabled = true, want false", tc.name)
		}

		// ExpandExec default
		argv, err := s.ExpandExec("")
		if err != nil {
			t.Fatalf("%s ExpandExec default: %v", tc.name, err)
		}
		if strings.Join(argv, " ") != tc.defaultArgv {
			t.Errorf("%s ExpandExec default = %q, want %q", tc.name, strings.Join(argv, " "), tc.defaultArgv)
		}

		// ExpandExec with overrides
		s.Ports[0].Port = tc.port + 1
		s.Ports[0].Bind = "0.0.0.0"
		s.Settings["data_dir"] = tc.customData
		argv, err = s.ExpandExec("")
		if err != nil {
			t.Fatalf("%s ExpandExec custom: %v", tc.name, err)
		}
		if strings.Join(argv, " ") != tc.customArgv {
			t.Errorf("%s ExpandExec custom = %q, want %q", tc.name, strings.Join(argv, " "), tc.customArgv)
		}
	}
}

func TestExternalServiceSeeds_PortConflictAndIdentity(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)

	// Round-trip show --yaml | add -f - identity for external seeds
	for _, name := range []string{"zot", "seaweedfs", "kopia"} {
		before := mustSvc(t, opts, "", "show", name, "--yaml")
		mustSvc(t, opts, before, "add", "-f", "-")
		after := mustSvc(t, opts, "", "show", name, "--yaml")
		if before != after {
			t.Fatalf("%s: show|add -f - is not identity:\n--- before\n%s--- after\n%s", name, before, after)
		}
		mustSvc(t, opts, "", "rm", name)
	}

	// Port conflict checks for each external seed when enabled
	conflicts := []struct {
		name string
		port int
	}{
		{"zot", 5000},
		{"seaweedfs", 8333},
		{"kopia", 51515},
	}
	for _, c := range conflicts {
		mustSvc(t, opts, "", "enable", c.name)
		clashYAML := fmt.Sprintf("name: fake-%s\nkind: service\nexec: [fake, --port, '{port}']\nports:\n  - port: %d\nenabled: true\n", c.name, c.port)
		out, err := svcRun(t, opts, clashYAML, "add", "-f", "-")
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf(`held by service %q`, c.name)) {
			t.Fatalf("add over %s's port: err=%v out=%s", c.name, err, out)
		}
		mustSvc(t, opts, "", "disable", c.name)
	}
}
