package fleet

import (
	"reflect"
	"strings"
	"testing"
)

// Sprint 418 S17 (story eae40495aac7): outpost's retained in-process
// surfaces are internal units. outpost registers the capability ids (S18,
// docs/settings.md "Contract notes for the unit seeds") and ships no unit
// YAML, so a seed naming the wrong id binds to nothing.
//
// Sprint 436 (story 1b4773cfbdd7) restored the sphere inference units to
// on-by-default: before Sprint 418 S18, OllamaOn() defaulted on and
// ShardOn() was on for any paired host.
//
// Sprint 437 (story a4300f7a000a) restores the remaining pre-418 defaults:
// before Sprint 418, outpost defaulted podman, files and otel (+ otel pool)
// on (PodmanOn/FilesOn/OtelOn nil=>true) and warm followed OllamaOn. The
// rest stay opt-in: sandbox and lan-inference were explicit opt-in, and
// actrunner and ycode ship disabled by history.

// outpostCapabilitySeeds maps each seed to the capability id outpost
// registers for it.
var outpostCapabilitySeeds = map[string]string{
	"actrunner":     "outpost.actrunner",
	"files":         "outpost.files",
	"lan-inference": "outpost.lan-inference",
	"ollama":        "outpost.ollama",
	"ollama-pool":   "outpost.ollama-pool",
	"otel":          "outpost.otel",
	"podman":        "outpost.podman",
	"sandbox":       "outpost.sandbox",
	"shard":         "outpost.shard",
	"warm":          "outpost.warm",
	"ycode":         "outpost.ycode",
}

// outpostDefaultOnSeeds are the units that ship on-by-default: the sphere
// inference units Sprint 436 restored (ollama, ollama-pool, shard) plus the
// pre-418 outpost defaults Sprint 437 restored (otel, warm, files, podman).
// Every other outpost capability seed stays opt-in.
var outpostDefaultOnSeeds = map[string]bool{
	"ollama":      true,
	"ollama-pool": true,
	"shard":       true,
	"otel":        true,
	"warm":        true,
	"files":       true,
	"podman":      true,
}

func TestOutpostCapabilitySeedsAreInternalUnits(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	for name, id := range outpostCapabilitySeeds {
		s, ok := cat.Service(name)
		if !ok {
			t.Errorf("%s seed missing", name)
			continue
		}
		if err := s.Validate(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if s.ProviderName() != ServiceProviderInternal || s.Capability != id || s.Owner != ServiceOwnerOutpost {
			t.Errorf("%s = provider %q capability %q owner %q, want internal %s owned by outpost",
				name, s.ProviderName(), s.Capability, s.Owner, id)
		}
		if outpostDefaultOnSeeds[name] {
			if !s.Enabled {
				t.Errorf("%s seeded disabled; paired hosts need this sphere unit by default", name)
			}
		} else if s.Enabled {
			t.Errorf("%s seeded enabled; enabling is the operator's", name)
		}
		if len(s.Exec) != 0 || s.Download != nil || s.Workdir != "" {
			t.Errorf("%s declares a process: %+v", name, s)
		}
		// Each capability publishes its own routes; a declared exposure
		// would publish a second copy of them.
		if s.Expose.App != nil || len(s.Expose.Mesh) != 0 {
			t.Errorf("%s declares exposure %+v", name, s.Expose)
		}
		if s.Restart.Policy != RestartOnFailure {
			t.Errorf("%s restart = %+v, want on-failure", name, s.Restart)
		}
	}
}

func TestOutpostCapabilitySeedsKeepSettingsAndSafetyDefaults(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	want := map[string]map[string]string{
		"files":         {"scope": "", "allow_write": "false"},
		"podman":        {"socket": ""},
		"sandbox":       {"socket": "", "max_memory_mb": "0", "cpus": "0", "pids_limit": "0", "max_containers": "0", "allowed_images": "", "scratch_dir": "", "prewarm_images": ""},
		"ollama":        {"url": ""},
		"warm":          {"budget_frac": "0.33"},
		"shard":         {"role": "auto", "peers": "", "node_bytes": "0", "server_bin": "", "worker_bin": ""},
		"lan-inference": {"target": ""},
		"otel":          {"pool": "true"},
		"ycode":         {"share": "", "require_login": "false"},
	}
	for name, settings := range want {
		s, _ := cat.Service(name)
		if !reflect.DeepEqual(s.Settings, settings) {
			t.Errorf("%s settings = %v, want %v", name, s.Settings, settings)
		}
	}
	for _, name := range []string{"files", "podman", "sandbox", "ollama", "ollama-pool", "warm", "shard", "otel", "ycode"} {
		s, _ := cat.Service(name)
		for k := range s.Settings {
			if strings.Contains(k, "token") || strings.Contains(k, "password") {
				t.Errorf("%s declares an inline credential setting %q", name, k)
			}
		}
	}
}

func TestOllamaPoolRequiresTheProxyUnit(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	p, _ := cat.Service("ollama-pool")
	if !reflect.DeepEqual(p.Dependencies(), []string{"ollama"}) {
		t.Fatalf("ollama-pool dependencies = %v, want [ollama]", p.Dependencies())
	}
	otel, _ := cat.Service("otel")
	if !reflect.DeepEqual(otel.Dependencies(), []string{"otel-stack"}) {
		t.Fatalf("otel dependencies = %v, want [otel-stack]", otel.Dependencies())
	}
	for name := range outpostCapabilitySeeds {
		if name == "ollama-pool" || name == "otel" {
			continue
		}
		if s, _ := cat.Service(name); len(s.Dependencies()) != 0 {
			t.Errorf("%s dependencies = %v, want none (outpost hosts it)", name, s.Dependencies())
		}
	}
}

func TestOTelStackSeedRunsLocalSignedStack(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	s, ok := cat.Service("otel-stack")
	if !ok {
		t.Fatal("otel-stack seed missing")
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	wantExec := []string{"bashy", "otel", "serve", "--bind", "127.0.0.1", "--port", "31415", "--otlp-grpc-port", "31416"}
	if !reflect.DeepEqual(s.Exec, wantExec) {
		t.Errorf("exec = %v, want %v", s.Exec, wantExec)
	}
	wantPorts := []ServicePort{{Name: "http", Port: 31415, Bind: "loopback"}, {Name: "grpc", Port: 31416, Bind: "loopback"}}
	if !reflect.DeepEqual(s.Ports, wantPorts) {
		t.Errorf("ports = %+v, want %+v", s.Ports, wantPorts)
	}
	if s.Health.HTTP != "/healthz" || s.Restart.Policy != RestartOnFailure ||
		s.Restart.StartLimit.Burst != 5 || s.Restart.StartLimit.Interval != "10m" {
		t.Errorf("health or restart = %+v %+v", s.Health, s.Restart)
	}
	if !s.Enabled {
		t.Error("otel-stack must be default-on")
	}
}

func TestLANInferenceDeclaresItsLANPortAndOthersNone(t *testing.T) {
	cat := New(serviceOpts(t.TempDir())...)
	l, _ := cat.Service("lan-inference")
	if !reflect.DeepEqual(l.Ports, []ServicePort{{Name: "http", Port: 11436, Bind: "lan"}}) {
		t.Fatalf("lan-inference ports = %+v, want http 11436 lan", l.Ports)
	}
	for name := range outpostCapabilitySeeds {
		if name == "lan-inference" {
			continue
		}
		if s, _ := cat.Service(name); len(s.Ports) != 0 {
			t.Errorf("%s declares ports %+v", name, s.Ports)
		}
	}
}

// The seeds are configured through the same verbs as every other unit.
func TestOutpostCapabilitySeedsTakeGenericOverrides(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	mustSvc(t, opts, "", "set", "files", "--set", "settings.allow_write=true", "--set", "settings.scope=/srv/shared")
	mustSvc(t, opts, "", "set", "lan-inference", "--set", "ports.0.port=11437")
	mustSvc(t, opts, "", "enable", "files")
	mustSvc(t, opts, "", "enable", "lan-inference")
	cat := New(opts...)
	f, _ := cat.Service("files")
	if !f.Enabled || f.Settings["allow_write"] != "true" || f.Settings["scope"] != "/srv/shared" {
		t.Fatalf("files after override = %+v", f)
	}
	if ov := matrixOverlay(t, root, dirServices, "files"); strings.Contains(ov, "capability:") {
		t.Fatalf("files override is not sparse:\n%s", ov)
	}
	if l, _ := cat.Service("lan-inference"); !l.Enabled || l.Ports[0].Port != 11437 {
		t.Fatalf("lan-inference after override = %+v", l)
	}
	if _, err := svcRun(t, opts, "", "set", "warm", "--set", "exec=[warm]"); err == nil {
		t.Fatal("exec accepted on an internal unit")
	}
	mustSvc(t, opts, "", "rm", "files")
	if f, _ := New(opts...).Service("files"); f.Ring.String() != "embedded" || !f.Enabled {
		t.Fatalf("files after rm = %+v, want the enabled embedded seed", f)
	}
}
