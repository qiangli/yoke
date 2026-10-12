package feed

import (
	"context"
	"fmt"
	"strings"
)

type rcloneRunner func(context.Context, ...string) ([]byte, error)

// checkRcloneRemote verifies a named remote before saving a sink. Bare local
// paths remain valid rclone targets and need no configuration.
func checkRcloneRemote(ctx context.Context, target string, run rcloneRunner) error {
	name, _, remote := strings.Cut(target, ":")
	if remote && name == "" {
		// :backend:path is rclone's on-the-fly backend form, not a
		// configured remote. In particular, :local:path needs no config.
		if backend, _, ok := strings.Cut(target[1:], ":"); ok && backend != "" {
			return nil
		}
		return fmt.Errorf("feed: rclone target %q needs a remote name; run `bashy rclone config` to add one", target)
	}
	if len(name) == 1 && ((name[0] >= 'A' && name[0] <= 'Z') || (name[0] >= 'a' && name[0] <= 'z')) &&
		len(target) > 2 && (target[2] == '/' || target[2] == '\\') {
		return nil
	}
	if !remote || strings.ContainsAny(name, `/\`) {
		return nil
	}
	out, err := run(ctx, "listremotes")
	if err != nil {
		return fmt.Errorf("feed: cannot list rclone remotes: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == name+":" {
			return nil
		}
	}
	return fmt.Errorf("feed: rclone remote %q is not configured; run `bashy rclone config` to add it", name)
}

// rclonePublicURL derives a published file's public URL. A configured public
// base takes precedence over backend links, which may be expiring presigned
// URLs. An empty result keeps the post out of the registry.
func rclonePublicURL(ctx context.Context, run rcloneRunner, dest, publicBase, relPath string) string {
	if publicBase != "" {
		return publicBase + "/" + relPath
	}
	if out, err := run(ctx, "link", dest); err == nil {
		if link := strings.TrimSpace(string(out)); isPublicSinkURL(link) {
			return link
		}
	}
	return ""
}
