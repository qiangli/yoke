package feed

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Sink is a destination a feed post can be published to.
type Sink interface {
	Name() string
	Kind() string
	Put(ctx context.Context, relPath string, data []byte) (url string, err error)
	Get(ctx context.Context, ref string) ([]byte, error)
}

// SinkConfig describes one sink. Kind is "github" (any git remote URL;
// GitHub repos/Pages in practice) or "rclone" (target "remote:path").
// Branch is the git branch for github sinks (default "main"); unused by
// rclone sinks.
type SinkConfig struct {
	Name   string `json:"name"`
	Kind   string `json:"kind"`
	Target string `json:"target"`
	Branch string `json:"branch,omitempty"`
}

// SinkResult is the outcome of publishing to one sink.
type SinkResult struct {
	Name string
	URL  string
	Err  error
}

// OpenSink builds the Sink for cfg, dispatching on cfg.Kind.
func OpenSink(cfg SinkConfig) (Sink, error) {
	switch cfg.Kind {
	case "github":
		return openGitSink(cfg)
	case "rclone":
		return openRcloneSink(cfg)
	default:
		return nil, fmt.Errorf("feed: unknown sink kind %q", cfg.Kind)
	}
}

// PutAll publishes data to every sink in order and returns one SinkResult
// per sink, in the same order. A failing sink never hides the others'
// success: every sink runs even when an earlier one fails.
func PutAll(ctx context.Context, sinks []Sink, relPath string, data []byte) []SinkResult {
	results := make([]SinkResult, 0, len(sinks))
	for _, s := range sinks {
		if s == nil {
			continue
		}
		url, err := s.Put(ctx, relPath, data)
		results = append(results, SinkResult{Name: s.Name(), URL: url, Err: err})
	}
	return results
}

func sinksFilePath(dir string) string {
	return filepath.Join(dir, "sinks.json")
}

func validateSinkConfigs(cfgs []SinkConfig) error {
	seen := make(map[string]struct{}, len(cfgs))
	for _, c := range cfgs {
		if c.Name == "" {
			return fmt.Errorf("feed: sink name is required")
		}
		if strings.ContainsAny(c.Name, `/\`) || c.Name == "." || c.Name == ".." {
			return fmt.Errorf("feed: invalid sink name %q", c.Name)
		}
		if c.Kind != "github" && c.Kind != "rclone" {
			return fmt.Errorf("feed: sink %q: unknown kind %q", c.Name, c.Kind)
		}
		if c.Target == "" {
			return fmt.Errorf("feed: sink %q: target is required", c.Name)
		}
		if _, dup := seen[c.Name]; dup {
			return fmt.Errorf("feed: duplicate sink name %q", c.Name)
		}
		seen[c.Name] = struct{}{}
	}
	return nil
}

// LoadSinks reads dir/sinks.json. A missing file means no sinks are
// configured and returns (nil, nil).
func LoadSinks(dir string) ([]SinkConfig, error) {
	data, err := os.ReadFile(sinksFilePath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("feed: load sinks: %w", err)
	}
	var cfgs []SinkConfig
	if err := json.Unmarshal(data, &cfgs); err != nil {
		return nil, fmt.Errorf("feed: load sinks: %w", err)
	}
	if err := validateSinkConfigs(cfgs); err != nil {
		return nil, err
	}
	return cfgs, nil
}

// SaveSinks validates cfgs (unique names, known kinds) and writes
// dir/sinks.json atomically (temp file + rename).
func SaveSinks(dir string, cfgs []SinkConfig) error {
	if err := validateSinkConfigs(cfgs); err != nil {
		return err
	}
	if cfgs == nil {
		cfgs = []SinkConfig{}
	}
	data, err := json.MarshalIndent(cfgs, "", "  ")
	if err != nil {
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	tmp, err := os.CreateTemp(dir, "sinks-*.json")
	if err != nil {
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	if err := os.Rename(tmpName, sinksFilePath(dir)); err != nil {
		os.Remove(tmpName)
		return fmt.Errorf("feed: save sinks: %w", err)
	}
	return nil
}

// validateRelPath cleans a sink-relative path and rejects anything that
// could escape the sink root: empty, absolute, or parent-traversing paths.
func validateRelPath(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("feed: path is empty")
	}
	if strings.ContainsRune(p, 0) {
		return "", fmt.Errorf("feed: path %q contains NUL", p)
	}
	slash := filepath.ToSlash(p)
	if path.IsAbs(slash) || filepath.IsAbs(p) {
		return "", fmt.Errorf("feed: absolute path %q not allowed", p)
	}
	if len(slash) >= 2 && slash[1] == ':' && ('a' <= slash[0] && slash[0] <= 'z' || 'A' <= slash[0] && slash[0] <= 'Z') {
		return "", fmt.Errorf("feed: path %q not allowed", p)
	}
	c := path.Clean(slash)
	if c == "." || c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("feed: path %q escapes the sink root", p)
	}
	return c, nil
}

// sinkBaseDir is the KeyDir-independent root all sink caches live under:
// $BASHY_HOME/feed/sinks (BASHY_HOME is resolved here because S2 owns
// KeyDir; falling back to ~/.bashy when BASHY_HOME is unset).
func sinkBaseDir() (string, error) {
	if h := os.Getenv("BASHY_HOME"); h != "" {
		return filepath.Join(h, "feed", "sinks"), nil
	}
	h, err := os.UserHomeDir()
	if err != nil || h == "" {
		return "", fmt.Errorf("feed: cannot resolve sink cache dir: set BASHY_HOME")
	}
	return filepath.Join(h, ".bashy", "feed", "sinks"), nil
}
