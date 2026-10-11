package fleet

import (
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/external/actrunner"
)

// Sprint 418 S17 (story d91c46d83416): the Headlamp port-forward and the
// act_runner CI executor are plain units — same verbs, no name-specific code.

func TestHeadlampSeedIsAGenericProcessUnit(t *testing.T) {
	h, ok := New(serviceOpts(t.TempDir())...).Service("headlamp")
	if !ok {
		t.Fatal("headlamp seed missing")
	}
	if h.ProviderName() != ServiceProviderProcess || h.Enabled {
		t.Fatalf("headlamp = %+v, want a disabled process unit", h)
	}
	argv, err := h.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	want := "bashy kubectl --kubeconfig= -n headlamp port-forward --address 127.0.0.1 svc/headlamp 18466:80"
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	if p, _ := h.PrimaryPort(); p.Port != 18466 || p.Bind != "loopback" || p.Name != "http" {
		t.Errorf("primary port = %+v, want http 18466 loopback", p)
	}
	if !h.Health.TCP || h.Health.HTTP != "" {
		t.Errorf("health = %+v, want a tcp probe", h.Health)
	}
	if len(h.Expose.Mesh) != 1 || h.Expose.Mesh[0].Name != "headlamp" || h.Expose.App != nil {
		t.Errorf("expose = %+v, want mesh headlamp only", h.Expose)
	}
	if h.Env["DKS_PROFILE"] != "peer" {
		t.Errorf("env = %v, want the peer plane pinned", h.Env)
	}
	if h.Restart.Policy != RestartOnFailure || h.Restart.Backoff.Min != "1s" || h.Restart.Backoff.Max != "30s" {
		t.Errorf("restart = %+v", h.Restart)
	}
	if !strings.Contains(h.Description, "outpost bundle install headlamp") {
		t.Errorf("description %q lost the install prerequisite", h.Description)
	}
	for _, a := range h.Exec {
		if a == "0.0.0.0" || a == "{bind}" {
			t.Errorf("exec %v must pin the forward to 127.0.0.1", h.Exec)
		}
	}
}

func TestHeadlampOverridesMoveArgvThroughSettingsAndPorts(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	mustSvc(t, opts, "", "set", "headlamp",
		"--set", "ports.0.port=18467",
		"--set", "settings.namespace=ops-ui",
		"--set", "settings.target=svc/headlamp-2",
		"--set", "settings.kubeconfig=/k/peer.yaml")
	h, _ := New(opts...).Service("headlamp")
	argv, err := h.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	want := "bashy kubectl --kubeconfig=/k/peer.yaml -n ops-ui port-forward --address 127.0.0.1 svc/headlamp-2 18467:80"
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("argv = %q, want %q", got, want)
	}
	ov := matrixOverlay(t, root, dirServices, "headlamp")
	if strings.Contains(ov, "exec:") {
		t.Fatalf("headlamp override is not sparse:\n%s", ov)
	}
}

func TestInfraSeedsShowYAMLAddFileIsIdentity(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	for _, name := range []string{"headlamp", "actrunner"} {
		before := mustSvc(t, opts, "", "show", name, "--yaml")
		mustSvc(t, opts, before, "add", "-f", "-")
		after := mustSvc(t, opts, "", "show", name, "--yaml")
		if before != after {
			t.Fatalf("%s: show|add -f - is not identity:\n--- before\n%s--- after\n%s", name, before, after)
		}
		mustSvc(t, opts, "", "verify", name)
		mustSvc(t, opts, "", "rm", name)
		if s, _ := New(opts...).Service(name); s.Ring.String() != "embedded" {
			t.Fatalf("%s ring after rm = %v, want the embedded seed", name, s.Ring)
		}
	}
}

func TestInfraSeedsEnableDisableAndSet(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	for _, name := range []string{"headlamp", "actrunner"} {
		mustSvc(t, opts, "", "enable", name)
		if s, _ := New(opts...).Service(name); !s.Enabled {
			t.Fatalf("%s not enabled", name)
		}
		mustSvc(t, opts, "", "disable", name)
		if s, _ := New(opts...).Service(name); s.Enabled {
			t.Fatalf("%s still enabled", name)
		}
	}
	mustSvc(t, opts, "", "set", "actrunner", "--set", "settings.sandbox=true", "--set", "settings.docker_host=unix:///run/podman.sock")
	a, _ := New(opts...).Service("actrunner")
	if a.Settings["sandbox"] != "true" || a.Settings["docker_host"] != "unix:///run/podman.sock" {
		t.Fatalf("settings = %v", a.Settings)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	mustSvc(t, opts, "", "set", "actrunner", "--require", "loom")
	if a, _ = New(opts...).Service("actrunner"); !reflect.DeepEqual(a.Dependencies(), []string{"loom"}) {
		t.Fatalf("dependencies = %v, want [loom]", a.Dependencies())
	}
}

func TestHeadlampPortConflictNamesHolder(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	mustSvc(t, opts, "", "enable", "headlamp")
	clash := "name: other-ui\nkind: service\nexec: [other-ui, --port, '{port}']\nports:\n  - port: 18466\nenabled: true\n"
	if _, err := svcRun(t, opts, clash, "add", "-f", "-"); err == nil || !strings.Contains(err.Error(), `held by service "headlamp"`) {
		t.Fatalf("add over headlamp's port: err=%v", err)
	}
	mustSvc(t, opts, "", "disable", "headlamp")
	mustSvc(t, opts, clash, "add", "-f", "-")
	if _, err := svcRun(t, opts, "", "enable", "headlamp"); err == nil || !strings.Contains(err.Error(), `"other-ui"`) {
		t.Fatalf("enable headlamp over other-ui's port: err=%v", err)
	}
	// A different port frees it.
	mustSvc(t, opts, "", "set", "headlamp", "--set", "ports.0.port=18468")
	mustSvc(t, opts, "", "enable", "headlamp")
}

func TestActrunnerSeedIsAnInternalCapabilityWithoutAProcess(t *testing.T) {
	a, ok := New(serviceOpts(t.TempDir())...).Service("actrunner")
	if !ok {
		t.Fatal("actrunner seed missing")
	}
	if a.ProviderName() != ServiceProviderInternal || a.Capability != "outpost.actrunner" || a.Owner != ServiceOwnerOutpost {
		t.Fatalf("actrunner = %+v, want internal outpost.actrunner owned by outpost", a)
	}
	if len(a.Exec) != 0 || a.Download != nil || a.Workdir != "" || len(a.Ports) != 0 || a.Expose.App != nil || len(a.Expose.Mesh) != 0 {
		t.Errorf("actrunner declares a process or exposure: %+v", a)
	}
	if got := a.Dependencies(); len(got) != 0 {
		t.Errorf("dependencies = %v, want none (outpost hosts it)", got)
	}
	if a.Enabled {
		t.Error("actrunner seeded enabled")
	}
	if a.Restart.Policy != RestartOnFailure || a.Restart.Backoff.Min != "5s" || a.Restart.Backoff.Max != "2m" {
		t.Errorf("restart = %+v, want on-failure 5s..2m", a.Restart)
	}
}

func TestActrunnerSeedSettingsMatchTheRunnerDefaults(t *testing.T) {
	a, _ := New(serviceOpts(t.TempDir())...).Service("actrunner")
	want := map[string]string{
		"labels":        actrunner.DefaultLabels,
		"sandbox_image": actrunner.DefaultSandboxImage,
		"forge_unit":    "loom",
		"sandbox":       "false",
		"instance":      "",
		"docker_host":   "",
	}
	for k, v := range want {
		got, present := a.Settings[k]
		if !present || got != v {
			t.Errorf("settings[%q] = %q (present=%v), want %q", k, got, present, v)
		}
	}
	if sb := actrunner.SandboxLabel(a.Settings["sandbox_image"]); sb != "sandbox:docker://docker.io/library/node:20-bookworm" {
		t.Errorf("sandbox label = %q", sb)
	}
}

func TestActrunnerTokenIsAVaultReferenceNeverInline(t *testing.T) {
	root := t.TempDir()
	opts := serviceOpts(root)
	a, _ := New(opts...).Service("actrunner")
	if !reflect.DeepEqual(a.Secrets, []ServiceSecret{{Env: "ACTRUNNER_TOKEN", Ref: "actrunner-registration-token", Optional: true}}) {
		t.Fatalf("secrets = %+v", a.Secrets)
	}
	if _, inline := a.Settings["token"]; inline {
		t.Error("seed declares an inline settings.token")
	}
	if _, inline := a.Env["ACTRUNNER_TOKEN"]; inline {
		t.Error("seed declares the token in env")
	}
	// A previously registered runner can restart without a registration token.
	// Fresh registration still receives the same vault reference when bound.
	shown := mustSvc(t, opts, "", "show", "actrunner", "--yaml")
	if !strings.Contains(shown, "optional: true") {
		t.Fatalf("show lost the optional registration-token contract:\n%s", shown)
	}
	// A secret entry without a vault ref is refused, and the shown record
	// carries the reference, never a value.
	bad := strings.Replace(shown, "ref: actrunner-registration-token", "ref: \"\"", 1)
	if _, err := svcRun(t, opts, bad, "add", "-f", "-"); err == nil {
		t.Fatal("secret without a vault ref accepted")
	}
	if out := mustSvc(t, opts, "", "show", "actrunner", "--yaml"); !strings.Contains(out, "ref: actrunner-registration-token") {
		t.Fatalf("show lost the vault reference:\n%s", out)
	}
}

func TestInternalActrunnerRefusesProcessFields(t *testing.T) {
	opts := serviceOpts(t.TempDir())
	if _, err := svcRun(t, opts, "", "set", "actrunner", "--set", "exec=[act_runner,daemon]"); err == nil {
		t.Fatal("exec accepted on an internal unit")
	}
}
