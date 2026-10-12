package feed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// The git sink shells out to the real git binary (resolved via
// exec.LookPath) instead of a pure-Go git implementation because
// publishing needs a full git: ref negotiation plus authenticated push
// over https/ssh with the user's credential helpers. Our own clone under
// $BASHY_HOME/feed/sinks/<name> is kept (fetch + reset to the remote
// branch), so this stays independent of sdlc.PublishGitHubPages, which is
// tied to SDLC runs.

type gitSink struct {
	name   string
	remote string
	branch string
	dir    string
}

func openGitSink(cfg SinkConfig) (Sink, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("feed: github sink name is required")
	}
	if strings.ContainsAny(cfg.Name, `/\`) || cfg.Name == "." || cfg.Name == ".." {
		return nil, fmt.Errorf("feed: github sink: invalid name %q", cfg.Name)
	}
	if cfg.Target == "" {
		return nil, fmt.Errorf("feed: github sink %q: target (git remote URL) is required", cfg.Name)
	}
	branch := cfg.Branch
	if branch == "" {
		branch = "main"
	}
	base, err := sinkBaseDir()
	if err != nil {
		return nil, err
	}
	return &gitSink{
		name:   cfg.Name,
		remote: cfg.Target,
		branch: branch,
		dir:    filepath.Join(base, cfg.Name),
	}, nil
}

func (s *gitSink) Name() string { return s.name }
func (s *gitSink) Kind() string { return "github" }

func gitBin() (string, error) {
	p, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("feed: git binary not found in PATH: %w", err)
	}
	return p, nil
}

func runGit(ctx context.Context, dir string, args ...string) ([]byte, error) {
	bin, err := gitBin()
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(stdout.String() + "\n" + stderr.String())
		if out == "" {
			out = err.Error()
		}
		return nil, fmt.Errorf("feed: git %s: %s", strings.Join(args, " "), out)
	}
	return stdout.Bytes(), nil
}

func gitRemoteRefExists(ctx context.Context, dir, ref string) bool {
	_, err := runGit(ctx, dir, "rev-parse", "--verify", "--quiet", ref)
	return err == nil
}

// sync ensures the cache clone exists and matches the remote branch.
func (s *gitSink) sync(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.dir, ".git")); err != nil {
		if err := os.MkdirAll(filepath.Dir(s.dir), 0o755); err != nil {
			return fmt.Errorf("feed: github sink %q: %w", s.name, err)
		}
		if _, err := runGit(ctx, "", "clone", s.remote, s.dir); err != nil {
			return err
		}
	}
	if _, err := runGit(ctx, s.dir, "fetch", "origin"); err != nil {
		return err
	}
	if gitRemoteRefExists(ctx, s.dir, "origin/"+s.branch) {
		if _, err := runGit(ctx, s.dir, "checkout", "-B", s.branch, "origin/"+s.branch); err != nil {
			return err
		}
	} else if _, err := runGit(ctx, s.dir, "checkout", "-B", s.branch); err != nil {
		return err
	}
	return nil
}

// Put writes data to relPath in the cache clone, commits
// "feed: post <relPath>" (skipped when already up to date), pushes the
// branch, and returns the public URL.
func (s *gitSink) Put(ctx context.Context, relPath string, data []byte) (string, error) {
	rp, err := validateRelPath(relPath)
	if err != nil {
		return "", err
	}
	if err := s.sync(ctx); err != nil {
		return "", err
	}
	full := filepath.Join(s.dir, filepath.FromSlash(rp))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", fmt.Errorf("feed: github sink %q: %w", s.name, err)
	}
	if err := os.WriteFile(full, data, 0o644); err != nil {
		return "", fmt.Errorf("feed: github sink %q: %w", s.name, err)
	}
	if _, err := runGit(ctx, s.dir, "add", "--", rp); err != nil {
		return "", err
	}
	st, err := runGit(ctx, s.dir, "status", "--porcelain", "--", rp)
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(st)) > 0 {
		if _, err := runGit(ctx, s.dir, "commit", "-m", "feed: post "+rp); err != nil {
			return "", err
		}
	}
	if _, err := runGit(ctx, s.dir, "push", "origin", s.branch); err != nil {
		return "", err
	}
	return buildGitURL(s.remote, s.branch, rp), nil
}

// Get reads ref (a relPath or a URL Put returned) from the cache clone
// after fetching the remote branch.
func (s *gitSink) Get(ctx context.Context, ref string) ([]byte, error) {
	if err := s.sync(ctx); err != nil {
		return nil, err
	}
	rp, err := validateRelPath(s.relFromRef(ref))
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, filepath.FromSlash(rp)))
	if err != nil {
		return nil, fmt.Errorf("feed: github sink %q: read %q: %w", s.name, rp, err)
	}
	return data, nil
}

// relFromRef maps a Put URL back to its relPath; anything else is treated
// as a relPath directly.
func (s *gitSink) relFromRef(ref string) string {
	if owner, repo, ok := parseGitHubRemote(s.remote); ok {
		prefix := "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + s.branch + "/"
		if strings.HasPrefix(ref, prefix) {
			return strings.TrimPrefix(ref, prefix)
		}
	}
	if prefix := s.remote + "#" + s.branch + ":"; strings.HasPrefix(ref, prefix) {
		return strings.TrimPrefix(ref, prefix)
	}
	return ref
}

// buildGitURL returns the public URL for a pushed file: for github.com
// remotes the raw file URL, otherwise "<remote>#<branch>:<relPath>".
func buildGitURL(remote, branch, relPath string) string {
	if owner, repo, ok := parseGitHubRemote(remote); ok {
		return "https://raw.githubusercontent.com/" + owner + "/" + repo + "/" + branch + "/" + relPath
	}
	return remote + "#" + branch + ":" + relPath
}

// parseGitHubRemote extracts owner/repo from https, ssh, or git@ style
// github.com remote URLs (a trailing .git is stripped).
func parseGitHubRemote(remote string) (owner, repo string, ok bool) {
	i := strings.Index(remote, "github.com")
	if i < 0 {
		return "", "", false
	}
	rest := remote[i+len("github.com"):]
	rest = strings.TrimPrefix(rest, ":")
	rest = strings.TrimPrefix(rest, "/")
	rest = strings.Trim(rest, "/")
	rest = strings.TrimSuffix(rest, ".git")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}
