package weave

import (
	"archive/tar"
	"bytes"
	"context"
	"github.com/qiangli/yoke/pkg/fleet"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSealedImagePlan(t *testing.T) {
	tool := fleet.Tool{Name: "agent-a", CLI: fleet.ToolCLI{Binary: "/host/bin/agent-a", Versions: []fleet.ToolVersion{{Version: "1.2.3", Install: "npm install -g @example/agent-a"}}}}
	plan, err := boothSealedPlanImage(tool, nil, "", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if plan.Tag != "localhost/bashy-sealed-agent-agent-a:1.2.3" {
		t.Fatal(plan.Tag)
	}
	for _, want := range []string{"FROM docker.io/library/debian:bookworm-slim", "git ca-certificates", "npm install -g -- '@example/agent-a@1.2.3'", "HOME=/home/booth"} {
		if !strings.Contains(plan.Containerfile, want) {
			t.Errorf("missing %q: %s", want, plan.Containerfile)
		}
	}
	if got := strings.Join(plan.Args, " "); got != "build --platform linux/arm64 --tag localhost/bashy-sealed-agent-agent-a:1.2.3 --file Containerfile -" {
		t.Fatal(got)
	}
	plan, err = boothSealedPlanImage(tool, nil, "localhost/custom:2", "amd64")
	if err != nil || plan.Tag != "localhost/custom:2" {
		t.Fatalf("%+v %v", plan, err)
	}
}

func TestSealedImageMissingRecipe(t *testing.T) {
	for _, install := range []string{"", "cp /host/agent-a /another/path", "npm install -g @example/agent-a && bad"} {
		tool := fleet.Tool{Name: "agent-a", CLI: fleet.ToolCLI{Binary: "agent-a", Versions: []fleet.ToolVersion{{Version: "1", Install: install}}}}
		_, err := boothSealedPlanImage(tool, nil, "", "amd64")
		if err == nil || !strings.Contains(err.Error(), "cli.versions.install") {
			t.Fatalf("install %q: %v", install, err)
		}
	}
}

func TestSealedImageDownload(t *testing.T) {
	tool := fleet.Tool{Name: "agent-a", CLI: fleet.ToolCLI{Binary: "agent-a"}}
	d := &fleet.CommandDownload{Version: "v2", URL: "https://example.test/{version}/{goos}-{goarch}.tar.gz", Member: "bin/agent-a", SHA256: map[string]string{"linux/arm64": strings.Repeat("a", 64)}}
	plan, err := boothSealedPlanImage(tool, d, "", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"https://example.test/v2/linux-arm64.tar.gz", "sha256sum -c", "tar -xOf", "bin/agent-a", "/usr/local/bin/agent-a"} {
		if !strings.Contains(plan.Containerfile, want) {
			t.Errorf("missing %q", want)
		}
	}
	delete(d.SHA256, "linux/arm64")
	if _, err = boothSealedPlanImage(tool, d, "", "arm64"); err == nil || !strings.Contains(err.Error(), "download.sha256.linux/arm64") {
		t.Fatal(err)
	}
}

func TestSealedLoginRequiresPrivateFiles(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "booth-7-home")
	if err := boothSealedCheckLogin("agent-a", dir, 7, []string{"HOME=" + home}); err != nil {
		t.Fatal(err)
	}
	err := boothSealedCheckLogin("agent-a", dir, 7, []string{"HOME=/host/home"})
	if err == nil || err.Error() != "sealed booth unsupported for agent-a: login is not file-based on this host" {
		t.Fatal(err)
	}
}

func TestSealedImageDefaultAndOverride(t *testing.T) {
	tool := fleet.Tool{Name: "agent-a", CLI: fleet.ToolCLI{Versions: []fleet.ToolVersion{{Version: "2.3"}}}}
	tag, err := boothSealedImageTag(tool, nil, "")
	if err != nil || tag != "localhost/bashy-sealed-agent-agent-a:2.3" {
		t.Fatalf("%s %v", tag, err)
	}
	t.Setenv("BASHY_SEALED_IMAGE", "localhost/custom:override")
	if got := boothSealedImageFor("agent-a"); got != "localhost/custom:override" {
		t.Fatal(got)
	}
}

func TestSealedImageDryRunUsesRegistry(t *testing.T) {
	cat := fleet.New(fleet.WithRoot(t.TempDir()))
	tool := fleet.Tool{Name: "agent-a", Kind: "cli", CLI: fleet.ToolCLI{Binary: "agent-a", Versions: []fleet.ToolVersion{{Version: "3.2", Install: "npm install -g @example/agent-a@3.2"}}}}
	if err := cat.SaveTool(tool); err != nil {
		t.Fatal(err)
	}
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { fleetCatalog = prev })
	t.Setenv("BASHY_SEALED_IMAGE", "")
	if got := boothSealedImageFor("agent-a"); got != "localhost/bashy-sealed-agent-agent-a:3.2" {
		t.Fatal(got)
	}
	var output bytes.Buffer
	cmd := newWeaveCmd()
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"sealed-image", "build", "--tool", "agent-a", "--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err, output.String())
	}
	for _, want := range []string{"Containerfile:", "npm install -g -- '@example/agent-a@3.2'", "podman argv: [", "Containerfile", "localhost/bashy-sealed-agent-agent-a:3.2"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("missing %q in %s", want, output.String())
		}
	}
	plan, err := boothSealedPlanImage(tool, nil, "", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	input, err := boothSealedImageContext(plan)
	if err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(strings.NewReader(input))
	h, err := reader.Next()
	if err != nil || h.Name != "Containerfile" {
		t.Fatalf("%v %v", h, err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != plan.Containerfile {
		t.Fatalf("%s %v", body, err)
	}
	if _, err := reader.Next(); err != io.EOF {
		t.Fatalf("extra build context: %v", err)
	}
}

func TestSealedKeychainSeedRefusedBeforePodman(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	env, err := boothSeedAgentDirs(nil, dir, 7, "claude")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = boothSealedStart(context.Background(), dir, &weaveItem{ID: 7, ArenaSprint: 331}, nil, "claude", "", nil, env, "", "", weaveStartOptions{})
	if err == nil || !strings.Contains(err.Error(), "login is not file-based on this host") {
		t.Fatal(err)
	}
}

func TestSealedFileLoginStagedAndTranslated(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".agent-a"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".agent-a", "auth.json"), []byte("private-login"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	env, err := boothSeedAgentDirs(nil, dir, 7, "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := boothSealedCheckLogin("agent-a", dir, 7, env); err != nil {
		t.Fatal(err)
	}
	stage, err := boothSealedStage(boothSealedInput{QueueDir: dir, Run: 7})
	if err != nil {
		t.Fatal(err)
	}
	mapped := boothSealedTranslateEnv(env, boothSealedPathMaps("/workspace", dir, 7))
	config := boothGetEnv(mapped, boothConfigEnv("agent-a"))
	if !strings.HasPrefix(config, boothSealedHome+"/") {
		t.Fatal(config)
	}
	rel := strings.TrimPrefix(config, boothSealedHome+"/")
	b, err := os.ReadFile(filepath.Join(stage, rel, "auth.json"))
	if err != nil || string(b) != "private-login" {
		t.Fatalf("%s %v", b, err)
	}
}
