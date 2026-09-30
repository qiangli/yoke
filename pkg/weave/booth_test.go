package weave

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func TestBoothScrubEnv(t *testing.T) {
	in := []string{"PATH=/bin", "GITHUB_TOKEN=secret", "GH_TOKEN=secret", "GIT_DIR=/real", "SSH_AUTH_SOCK=/sock", "BASHY_SPRINT_LEASE_TOKEN=secret", "BASHY_PRINCIPAL=host", "CUSTOM_SECRET=secret", "OPENAI_API_KEY=keep"}
	got := strings.Join(boothScrubEnv(in, []string{"CUSTOM_SECRET"}), "\n")
	for _, key := range []string{"GITHUB_TOKEN=", "GH_TOKEN=", "GIT_DIR=", "SSH_AUTH_SOCK=", "BASHY_SPRINT_LEASE_TOKEN=", "BASHY_PRINCIPAL=", "CUSTOM_SECRET="} {
		if strings.Contains(got, key) {
			t.Errorf("retained %s", key)
		}
	}
	if !strings.Contains(got, "OPENAI_API_KEY=keep") || !strings.Contains(got, "PATH=/bin") {
		t.Fatalf("lost needed env: %s", got)
	}
}

func TestBoothWorkspaceOriginOnlyFork(t *testing.T) {
	root := filepath.Join(t.TempDir(), "real-repo")
	fork := filepath.Join(t.TempDir(), "private-fork")
	r, err := gogit.PlainInit(fork, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateRemote(&config.RemoteConfig{Name: "upstream", URLs: []string{root}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fork, "story.txt"), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := r.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Add("story.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Commit("base", &gogit.CommitOptions{Author: &object.Signature{Name: "agent-a", Email: "a@example.invalid", When: time.Now()}}); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(t.TempDir(), "workspace")
	cmd := exec.Command("git", boothCloneArgs(fork, "booth-4-7", filepath.Join(t.TempDir(), "credentials"), workspace)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone: %s: %v", output, err)
	}
	config, err := os.ReadFile(filepath.Join(workspace, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(config), fork) || strings.Contains(string(config), root) {
		t.Fatalf("wrong origin: %s", config)
	}
	if count := strings.Count(string(config), `[remote "`); count != 1 {
		t.Fatalf("remote count %d: %s", count, config)
	}
}

func TestBoothCloneURLHasUserWithoutPassword(t *testing.T) {
	args := strings.Join(boothCloneArgs("http://127.0.0.1:3000/booth-4-7/repo.git", "booth-4-7", "/tmp/credentials", "/tmp/workspace"), " ")
	if !strings.Contains(args, "http://booth-4-7@127.0.0.1:3000/booth-4-7/repo.git") || strings.Contains(args, ":secret@") {
		t.Fatalf("clone args %s", args)
	}
}

type boothFakeBackend struct{ user, fork string }

func (b *boothFakeBackend) CreateBooth(_, _, user, _ string) (string, error) {
	b.user = user
	return "http://127.0.0.1/" + user + "/repo.git", nil
}
func (b *boothFakeBackend) CopyBoothBase(_, _, fork, _, _ string) error { b.fork = fork; return nil }

func TestBoothPrepareFakeAndNoSecretInItem(t *testing.T) {
	dir := t.TempDir()
	fake := new(boothFakeBackend)
	user, fork, cred, err := boothPrepare(dir, "repo", 7, "sprint-4-12345678", fake)
	if err != nil {
		t.Fatal(err)
	}
	if user != "booth-4-12345678-7" || fake.user != user || fake.fork != fork {
		t.Fatalf("provision: %q %q %q", user, fake.user, fake.fork)
	}
	pass, err := os.ReadFile(filepath.Join(dir, "booth-7.password"))
	if err != nil {
		t.Fatal(err)
	}
	passInfo, err := os.Stat(filepath.Join(dir, "booth-7.password"))
	if err != nil || passInfo.Mode().Perm() != 0o600 {
		t.Fatalf("password mode: %v %v", passInfo, err)
	}
	item, err := json.Marshal(weaveItem{ArenaSprint: 4, BoothUser: user, BoothForkURL: fork, Blind: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(item), strings.TrimSpace(string(pass))) {
		t.Fatal("password in item")
	}
	st, err := os.Stat(cred)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode %o", st.Mode().Perm())
	}
	_, _, _, err = boothPrepare(dir, "repo", 7, "sprint-4-12345678", fake)
	if err != nil {
		t.Fatal(err)
	}
	passAgain, _ := os.ReadFile(filepath.Join(dir, "booth-7.password"))
	if string(passAgain) != string(pass) {
		t.Fatal("retry changed password")
	}
}

func TestBoothAgentDirCopiesLoginOnly(t *testing.T) {
	src := t.TempDir()
	queue := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "auth.json"), []byte("login"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "memory.json"), []byte("secret memory"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := boothSeedAgentDirs([]string{"TOOL_HOME=" + src}, queue, 3, "tool")
	if err != nil {
		t.Fatal(err)
	}
	var dest string
	for _, kv := range env {
		if strings.HasPrefix(kv, "TOOL_HOME=") {
			dest = strings.TrimPrefix(kv, "TOOL_HOME=")
		}
	}
	if dest == src {
		t.Fatal("agent dir was not redirected")
	}
	b, err := os.ReadFile(filepath.Join(dest, "auth.json"))
	if err != nil || string(b) != "login" {
		t.Fatalf("login copy: %q %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dest, "memory.json")); !os.IsNotExist(err) {
		t.Fatalf("memory leaked: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "auth.json"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(src, "auth.json"))
	if string(b) != "login" {
		t.Fatal("login was linked")
	}
}

func TestBoothAgyCopiesGeminiOAuthWithoutMemory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	src := filepath.Join(home, ".gemini")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"oauth_creds.json": "oauth", "google_accounts.json": "account", "history.json": "history"} {
		if err := os.WriteFile(filepath.Join(src, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(src, "antigravity-cli"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "antigravity-cli", "antigravity-oauth-token"), []byte("token"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := boothSeedAgentDirs([]string{"HOME=" + home}, t.TempDir(), 9, "agy")
	if err != nil {
		t.Fatal(err)
	}
	gemini := filepath.Join(boothGetEnv(env, "HOME"), ".gemini")
	for name, want := range map[string]string{"oauth_creds.json": "oauth", "google_accounts.json": "account"} {
		b, err := os.ReadFile(filepath.Join(gemini, name))
		if err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
	if _, err := os.Stat(filepath.Join(gemini, "history.json")); !os.IsNotExist(err) {
		t.Fatalf("history copied: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(gemini, "antigravity-cli", "antigravity-oauth-token"))
	if err != nil || string(b) != "token" {
		t.Fatalf("CLI token: %q %v", b, err)
	}
}

func TestBoothKeychainLoginKeepsConfigAndDisablesMemory(t *testing.T) {
	src := filepath.Join(t.TempDir(), ".claude")
	queue := t.TempDir()
	env, err := boothSeedAgentDirs([]string{"CLAUDE_CONFIG_DIR=" + src, "HOME=/real", "CLAUDE_CODE_DISABLE_AUTO_MEMORY=0"}, queue, 3, "claude")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		values[k] = v
	}
	if values["CLAUDE_CONFIG_DIR"] != "" {
		t.Fatalf("default keychain config should be implicit: %q", values["CLAUDE_CONFIG_DIR"])
	}
	if values["CLAUDE_CODE_DISABLE_AUTO_MEMORY"] != "1" {
		t.Fatalf("auto memory remains enabled: %q", values["CLAUDE_CODE_DISABLE_AUTO_MEMORY"])
	}
	if values["HOME"] != filepath.Dir(src) {
		t.Fatalf("keychain home changed: %q", values["HOME"])
	}
}

func TestBoothBlindArgvDisablesSessionPersistence(t *testing.T) {
	l := &weaveAgentLaunch{ToolName: "claude", Args: []string{"claude", "-p", "{prompt}"}}
	argv := boothBlindArgv(l, "story")
	if !strings.Contains(strings.Join(argv, " "), "--no-session-persistence") {
		t.Fatalf("persistent session: %v", argv)
	}
	if !strings.Contains(strings.Join(argv, " "), "--safe-mode") {
		t.Fatalf("global customization remains enabled: %v", argv)
	}
}

func TestBoothXDGDataIsPrivateAndCopiesLogin(t *testing.T) {
	src := t.TempDir()
	queue := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "opencode"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "opencode", "auth.json"), []byte("login"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := boothSeedAgentDirs([]string{"XDG_DATA_HOME=" + src, "XDG_CONFIG_HOME=/global/config", "XDG_STATE_HOME=/global/state"}, queue, 4, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		values[k] = v
	}
	for _, key := range []string{"XDG_DATA_HOME", "XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME"} {
		if !strings.HasPrefix(values[key], queue) {
			t.Errorf("%s escaped booth: %q", key, values[key])
		}
	}
	b, err := os.ReadFile(filepath.Join(values["XDG_DATA_HOME"], "opencode", "auth.json"))
	if err != nil || string(b) != "login" {
		t.Fatalf("login copy: %q %v", b, err)
	}
}

func TestBoothXDGConfigCopiesLogin(t *testing.T) {
	src := t.TempDir()
	queue := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "muse"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "muse", "auth.json"), []byte("login"), 0o600); err != nil {
		t.Fatal(err)
	}
	env, err := boothSeedAgentDirs([]string{"XDG_CONFIG_HOME=" + src}, queue, 5, "muse")
	if err != nil {
		t.Fatal(err)
	}
	config := boothGetEnv(env, "XDG_CONFIG_HOME")
	b, err := os.ReadFile(filepath.Join(config, "muse", "auth.json"))
	if err != nil || string(b) != "login" {
		t.Fatalf("login copy: %q %v", b, err)
	}
}

func TestBoothCredentialAndProjection(t *testing.T) {
	dir := t.TempDir()
	path, err := boothCredentialFile(dir, 7, "http://127.0.0.1:3000/booth-4-7/repo.git", "booth-4-7", "secret")
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", st.Mode().Perm())
	}
	if strings.Contains(boothProjection("Implement parser\nAssignee: agent-a\nHeat: 2\nSprint: #4\nSpec-ref: private"), "agent-a") {
		t.Fatal("assignee leaked")
	}
	if got := boothProjection("Implement parser\n- Assignee: agent-a\n**Heat:** 2\nSprint: #4\nSpec-ref: private"); got != "Implement parser" {
		t.Fatalf("projection %q", got)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("credential outside queue: %s", path)
	}
}
