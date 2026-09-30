package weave

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/external/loom"
)

type boothBackend interface {
	CreateBooth(org, repo, user, password string) (string, error)
	CopyBoothBase(org, repo, forkURL, user, password string) error
}

func boothSyncSiblingDeps(root, workspace string, allowed bool) ([]string, []string) {
	if !allowed {
		return nil, nil
	}
	return weaveSyncSiblingDeps(root, workspace)
}

func boothCloneArgs(fork, user, credentialFile, workspace string) []string {
	cloneURL := fork
	if u, err := url.Parse(fork); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
		u.User = url.User(user)
		cloneURL = u.String()
	}
	return []string{"-c", "credential.helper=store --file=" + credentialFile, "clone", "--no-checkout", cloneURL, workspace}
}

func boothScrubEnv(in []string, deny []string) []string {
	// The operator may add exact names through BASHY_BOOTH_ENV_DENY.
	blocked := map[string]bool{"GITHUB_TOKEN": true, "GH_TOKEN": true, "SSH_AUTH_SOCK": true, "BASHY_SPRINT_LEASE_TOKEN": true}
	for _, key := range deny {
		blocked[strings.TrimSpace(key)] = true
	}
	out := make([]string, 0, len(in))
	for _, kv := range in {
		key, _, ok := strings.Cut(kv, "=")
		if !ok || blocked[key] || strings.HasPrefix(key, "GIT_") || strings.HasPrefix(key, "GH_") || strings.HasPrefix(key, "GITHUB_") || (strings.HasPrefix(key, "BASHY_") && key != "BASHY_HOME") || strings.HasPrefix(key, "WEAVE_") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func boothProjection(body string) string {
	var lines []string
	for _, line := range strings.Split(body, "\n") {
		key := strings.ToLower(strings.TrimLeft(line, " \t-*#>"))
		key = strings.ReplaceAll(key, "*", "")
		if strings.HasPrefix(key, "assignee:") || strings.HasPrefix(key, "assigned to:") || strings.HasPrefix(key, "heat:") || strings.HasPrefix(key, "heat members:") || strings.HasPrefix(key, "sprint:") || strings.HasPrefix(key, "sprint card:") || strings.HasPrefix(key, "spec-ref:") || strings.HasPrefix(key, "story-id:") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func boothBlindArgv(l *weaveAgentLaunch, prompt string) []string {
	if l == nil {
		return nil
	}
	argv := l.Argv(prompt)
	// Keychain login requires the existing config identity. Print mode can
	// avoid writing a transcript there even though that config is shared.
	if boothConfigEnv(l.ToolName) == "CLAUDE_CONFIG_DIR" {
		if len(argv) > 0 && argv[len(argv)-1] == prompt {
			argv = append(argv[:len(argv)-1], "--safe-mode", "--no-session-persistence", prompt)
		} else {
			argv = append(argv, "--safe-mode", "--no-session-persistence")
		}
	}
	return weaveAgentEventsStdoutArgv(l, argv)
}

func boothConfigEnv(toolName string) string {
	toolName = strings.ToUpper(filepath.Base(toolName))
	var prefix strings.Builder
	for _, r := range toolName {
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			prefix.WriteRune(r)
		} else {
			prefix.WriteByte('_')
		}
	}
	return prefix.String() + "_CONFIG_DIR"
}

func boothSetEnv(env []string, key, value string) []string {
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return append(out, key+"="+value)
}

func boothUnsetEnv(env []string, key string) []string {
	out := env[:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func boothGetEnv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		if strings.HasPrefix(env[i], key+"=") {
			return strings.TrimPrefix(env[i], key+"=")
		}
	}
	return ""
}

func boothCredentialPath(queueDir string, run int64) string {
	return filepath.Join(queueDir, "booth-"+strconv.FormatInt(run, 10)+".credentials")
}

func boothCredentialFile(queueDir string, run int64, forkURL, user, password string) (string, error) {
	u, err := url.Parse(forkURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid booth fork URL")
	}
	u.User = url.UserPassword(user, password)
	path := boothCredentialPath(queueDir, run)
	if err := os.MkdirAll(queueDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(path, []byte(u.String()+"\n"), 0o600); err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func boothPasswordFile(queueDir string, run int64) (string, string, error) {
	path := filepath.Join(queueDir, "booth-"+strconv.FormatInt(run, 10)+".password")
	if b, err := os.ReadFile(path); err == nil {
		st, err := os.Lstat(path)
		if err != nil {
			return "", "", err
		}
		pass := strings.TrimSpace(string(b))
		if !st.Mode().IsRegular() || st.Mode().Perm() != 0o600 || len(pass) != 64 {
			return "", "", fmt.Errorf("invalid booth credential file")
		}
		if _, err := hex.DecodeString(pass); err != nil {
			return "", "", fmt.Errorf("invalid booth credential file")
		}
		return pass, path, nil
	} else if !os.IsNotExist(err) {
		return "", "", err
	}
	if err := os.MkdirAll(queueDir, 0o700); err != nil {
		return "", "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	pass := hex.EncodeToString(b)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", err
	}
	if _, err = f.WriteString(pass + "\n"); err != nil {
		_ = f.Close()
		return "", "", err
	}
	if err = f.Close(); err != nil {
		return "", "", err
	}
	return pass, path, nil
}

func boothPrepare(queueDir, repo string, run int64, org string, backend boothBackend) (string, string, string, error) {
	password, _, err := boothPasswordFile(queueDir, run)
	if err != nil {
		return "", "", "", err
	}
	suffix := strings.TrimPrefix(org, "sprint-")
	if suffix == org || suffix == "" {
		return "", "", "", fmt.Errorf("invalid arena org")
	}
	user := fmt.Sprintf("booth-%s-%d", suffix, run)
	fork, err := backend.CreateBooth(org, repo, user, password)
	if err != nil {
		return "", "", "", err
	}
	if err := backend.CopyBoothBase(org, repo, fork, user, password); err != nil {
		return "", "", "", err
	}
	cred, err := boothCredentialFile(queueDir, run, fork, user, password)
	return user, fork, cred, err
}

func boothLiveBackend() (boothBackend, error) {
	st, running, err := loom.ArenaState(context.Background())
	if err != nil {
		return nil, err
	}
	if !running {
		return nil, fmt.Errorf("loom arena is not running")
	}
	return loom.ArenaClient{URL: st.URL}, nil
}

func boothSeedAgentDirs(env []string, queueDir string, run int64, toolName string) ([]string, error) {
	// Bus and KB have no agent write kill switch. A fresh HOME plus BASHY_HOME
	// keeps their default stores and the tool's session/memory out of the host.
	home, _ := os.UserHomeDir()
	toolName = strings.ToLower(filepath.Base(toolName))
	if toolName == "" {
		return env, nil
	}
	boothHome := filepath.Join(queueDir, fmt.Sprintf("booth-%d-home", run))
	if err := os.MkdirAll(boothHome, 0o700); err != nil {
		return nil, err
	}
	defaultSource := filepath.Join(home, "."+toolName)
	defaultDest := filepath.Join(boothHome, "."+toolName)
	configKey := boothConfigEnv(toolName)
	// Auth behavior is keyed by the redirect variable, not a model or vendor.
	// This login uses the OS keychain, whose entry is scoped to the config dir.
	// Keep that identity while suppressing automatic memory and (in blind
	// print mode) session persistence. Other config writes remain possible;
	// standard booths are an integrity layer, not a hard filesystem sandbox.
	keychainLogin := configKey == "CLAUDE_CONFIG_DIR"
	if !keychainLogin {
		if err := boothCopyLogin(defaultSource, defaultDest); err != nil {
			return nil, err
		}
	}
	env = boothSetEnv(env, "HOME", boothHome)
	var prefix strings.Builder
	for _, r := range toolName {
		if r >= 'a' && r <= 'z' {
			prefix.WriteRune(r - ('a' - 'A'))
		} else if r >= '0' && r <= '9' {
			prefix.WriteRune(r)
		} else {
			prefix.WriteByte('_')
		}
	}
	for _, key := range []string{prefix.String() + "_HOME", configKey} {
		var source string
		source = boothGetEnv(env, key)
		if source == "" {
			source = defaultSource
		}
		if keychainLogin && key == configKey {
			env = boothSetEnv(env, key, source)
			continue
		}
		dest := filepath.Join(queueDir, "booth-"+strconv.FormatInt(run, 10)+"-agent", strings.ToLower(key))
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return nil, err
		}
		if err := boothCopyLogin(source, dest); err != nil {
			return nil, err
		}
		env = boothSetEnv(env, key, dest)
	}
	if keychainLogin {
		env = boothSetEnv(env, "CLAUDE_CODE_DISABLE_AUTO_MEMORY", "1")
		if filepath.Base(boothGetEnv(env, configKey)) == "."+toolName {
			// An explicit redirect to the default directory is still a
			// different keychain identity. Restore the default HOME lookup.
			source := boothGetEnv(env, configKey)
			env = boothSetEnv(env, "HOME", filepath.Dir(source))
			env = boothUnsetEnv(env, configKey)
		}
		fmt.Fprintf(os.Stderr, "weave: WARNING booth shares the keychain login home; automatic memory is disabled, but other tool settings may be written there\n")
	}
	// XDG locations can override HOME. Keep session databases and caches in
	// booth state even when the operator exports global XDG paths.
	xdgDataSource := boothGetEnv(env, "XDG_DATA_HOME")
	if xdgDataSource == "" {
		xdgDataSource = filepath.Join(home, ".local", "share")
	}
	xdgConfigSource := boothGetEnv(env, "XDG_CONFIG_HOME")
	if xdgConfigSource == "" {
		xdgConfigSource = filepath.Join(home, ".config")
	}
	for key, suffix := range map[string]string{
		"XDG_DATA_HOME":   filepath.Join(".local", "share"),
		"XDG_CONFIG_HOME": ".config",
		"XDG_CACHE_HOME":  ".cache",
		"XDG_STATE_HOME":  filepath.Join(".local", "state"),
	} {
		dest := filepath.Join(boothHome, suffix)
		if err := os.MkdirAll(dest, 0o700); err != nil {
			return nil, err
		}
		env = boothSetEnv(env, key, dest)
	}
	if configKey == "OPENCODE_CONFIG_DIR" {
		// This login is stored in XDG data, not the config directory.
		if err := boothCopyLogin(filepath.Join(xdgDataSource, toolName), filepath.Join(boothHome, ".local", "share", toolName)); err != nil {
			return nil, err
		}
	}
	if configKey == "MUSE_CONFIG_DIR" {
		if err := boothCopyLogin(filepath.Join(xdgConfigSource, toolName), filepath.Join(boothHome, ".config", toolName)); err != nil {
			return nil, err
		}
	}
	if configKey == "AGY_CONFIG_DIR" {
		// This CLI keeps browser OAuth in the shared SDK home, separate from
		// its own session directory. Copy only login files into private HOME.
		if err := boothCopyLogin(filepath.Join(home, ".gemini"), filepath.Join(boothHome, ".gemini")); err != nil {
			return nil, err
		}
		if err := boothCopyLogin(filepath.Join(home, ".gemini", "antigravity-cli"), filepath.Join(boothHome, ".gemini", "antigravity-cli")); err != nil {
			return nil, err
		}
	}
	return env, nil
}

func boothCopyLogin(source, dest string) error {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return err
	}
	for _, name := range []string{"auth.json", ".credentials.json", "oauth_creds.json", "google_accounts.json", "antigravity-oauth-token"} {
		b, err := os.ReadFile(filepath.Join(source, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dest, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}
