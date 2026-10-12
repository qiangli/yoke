package feed

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/qiangli/yoke/external/rclone"
)

type rcloneSink struct {
	name   string
	target string
	public string
	run    func(context.Context, ...string) ([]byte, error)
}

func openRcloneSink(cfg SinkConfig) (Sink, error) {
	return openRcloneSinkWithRunner(cfg, runRclone)
}

func openRcloneSinkWithRunner(cfg SinkConfig, run func(context.Context, ...string) ([]byte, error)) (Sink, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("feed: rclone sink name is required")
	}
	if strings.ContainsAny(cfg.Name, `/\`) || cfg.Name == "." || cfg.Name == ".." {
		return nil, fmt.Errorf("feed: rclone sink: invalid name %q", cfg.Name)
	}
	if cfg.Target == "" {
		return nil, fmt.Errorf("feed: rclone sink %q: target (remote:path) is required", cfg.Name)
	}
	public, err := normalizePublicURL(cfg.PublicURL)
	if err != nil {
		return nil, fmt.Errorf("feed: rclone sink %q: %w", cfg.Name, err)
	}
	return &rcloneSink{name: cfg.Name, target: cfg.Target, public: public, run: run}, nil
}

func (s *rcloneSink) Name() string { return s.name }
func (s *rcloneSink) Kind() string { return "rclone" }

func (s *rcloneSink) dest(relPath string) string {
	return strings.TrimSuffix(s.target, "/") + "/" + relPath
}

func runRclone(ctx context.Context, args ...string) ([]byte, error) {
	bin, err := rclone.Path(ctx, "")
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		out := strings.TrimSpace(stderr.String() + "\n" + stdout.String())
		if out == "" {
			out = err.Error()
		}
		return nil, fmt.Errorf("feed: rclone %s: %s", strings.Join(args, " "), out)
	}
	return stdout.Bytes(), nil
}

// Put uploads data to <target>/<relPath> via `rclone copyto`, then tries
// `rclone link` for a public URL. Remotes without link support (e.g. local
// paths) fall back to the "<target>/<relPath>" path as the URL — that is a
// usable ref for Get, not an error.
func (s *rcloneSink) Put(ctx context.Context, relPath string, data []byte) (string, error) {
	rp, err := validateRelPath(relPath)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp("", "feed-rclone-put-*")
	if err != nil {
		return "", fmt.Errorf("feed: rclone sink %q: temp file: %w", s.name, err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", fmt.Errorf("feed: rclone sink %q: temp file: %w", s.name, err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("feed: rclone sink %q: temp file: %w", s.name, err)
	}
	dest := s.dest(rp)
	if _, err := s.run(ctx, "copyto", tmpName, dest); err != nil {
		return "", err
	}
	if u := rclonePublicURL(ctx, s.run, dest, s.public, rp); u != "" {
		return u, nil
	}
	return dest, nil
}

// Get downloads ref via `rclone cat`. ref is a relPath or the
// "<target>/<relPath>" URL Put returned; a public `rclone link` URL cannot
// be mapped back to an rclone path, so use the relPath for those.
func (s *rcloneSink) Get(ctx context.Context, ref string) ([]byte, error) {
	base := strings.TrimSuffix(s.target, "/")
	dest := ref
	switch {
	case strings.HasPrefix(ref, s.public+"/") && s.public != "":
		rp, err := validateRelPath(strings.TrimPrefix(ref, s.public+"/"))
		if err != nil {
			return nil, err
		}
		dest = base + "/" + rp
	case strings.HasPrefix(ref, base+"/") || ref == s.target:
		dest = ref
	case strings.Contains(ref, "://"):
		return nil, fmt.Errorf("feed: rclone sink %q: cannot fetch public link %q; use the relPath", s.name, ref)
	default:
		rp, err := validateRelPath(ref)
		if err != nil {
			return nil, err
		}
		dest = base + "/" + rp
	}
	out, err := s.run(ctx, "cat", dest)
	if err != nil {
		return nil, err
	}
	return out, nil
}
