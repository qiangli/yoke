package actrunner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpec(t *testing.T) {
	s := Spec("")
	if s.Name != "act_runner" || s.Version != DefaultVersion {
		t.Fatalf("default spec = %+v", s)
	}
	if !strings.Contains(s.URLTemplate, "dl.gitea.com/act_runner") {
		t.Fatalf("url template not dl.gitea.com: %s", s.URLTemplate)
	}
	if !strings.Contains(s.URLTemplate, "{version}") || !strings.Contains(s.URLTemplate, "{goos}") {
		t.Fatalf("url template missing tokens: %s", s.URLTemplate)
	}
	if Spec("0.2.99").Version != "0.2.99" {
		t.Fatal("version override not honored")
	}
}

func TestRegisteredAndDataDir(t *testing.T) {
	dir := t.TempDir()
	if Registered(dir) {
		t.Fatal("empty dir should not be registered")
	}
	if err := os.WriteFile(filepath.Join(dir, ".runner"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !Registered(dir) {
		t.Fatal(".runner present should be registered")
	}
	t.Setenv("ACT_RUNNER_DIR", dir)
	if DefaultDataDir() != dir {
		t.Fatalf("DefaultDataDir = %s, want %s", DefaultDataDir(), dir)
	}
}

func TestSandboxLabel(t *testing.T) {
	// default image when empty
	if got := SandboxLabel(""); got != SandboxLabelName+":docker://"+DefaultSandboxImage {
		t.Fatalf("default SandboxLabel = %q", got)
	}
	// explicit image
	if got := SandboxLabel("docker.io/library/alpine:3.20"); got != "sandbox:docker://docker.io/library/alpine:3.20" {
		t.Fatalf("explicit SandboxLabel = %q", got)
	}
}

func TestRegisterOptionsSandboxLabels(t *testing.T) {
	// --sandbox appends the sandbox docker-executor label to the host default,
	// so one runner offers BOTH the tier-1 host build lane and the tier-3 container lane.
	o := RegisterOptions{Sandbox: true}
	o.defaults()
	if !strings.Contains(o.Labels, "host:host") {
		t.Fatalf("host label dropped: %q", o.Labels)
	}
	if !strings.Contains(o.Labels, "sandbox:docker://"+DefaultSandboxImage) {
		t.Fatalf("sandbox label missing: %q", o.Labels)
	}
	// custom image honored
	o2 := RegisterOptions{Sandbox: true, SandboxImage: "docker.io/library/node:22-bookworm"}
	o2.defaults()
	if !strings.Contains(o2.Labels, "sandbox:docker://docker.io/library/node:22-bookworm") {
		t.Fatalf("custom sandbox image not used: %q", o2.Labels)
	}
	// idempotent: a caller-supplied Labels already containing sandbox isn't doubled
	o3 := RegisterOptions{Sandbox: true, Labels: "host:host,sandbox:docker://x"}
	o3.defaults()
	if strings.Count(o3.Labels, "sandbox:") != 1 {
		t.Fatalf("sandbox label doubled: %q", o3.Labels)
	}
}

func TestSandboxConfig(t *testing.T) {
	dir := t.TempDir()
	if err := writeSandboxConfig(dir); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(ConfigPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	// The load-bearing line: docker_host "-" (connect via DOCKER_HOST env, don't
	// bind-mount the socket into the job — required for a podman-machine backend).
	if !strings.Contains(string(b), `docker_host: "-"`) {
		t.Fatalf("sandbox config missing docker_host \"-\":\n%s", b)
	}
	if ConfigPath(dir) != filepath.Join(dir, "config.yaml") {
		t.Fatalf("ConfigPath = %s", ConfigPath(dir))
	}
}
