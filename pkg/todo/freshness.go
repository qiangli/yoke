package todo

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RepoFreshness is the one line a git-shared view prints so a stale view
// says so and names the command (Sprint 217: views are per host; anything
// shared must show how fresh it is). It reads only what git already has —
// FETCH_HEAD's mtime for the last fetch, the upstream ref for ahead/behind —
// and never touches the network. Empty when root is not a git checkout with
// an upstream, so a view outside git prints nothing.
//
//	origin/main +2/-5 · last fetch 3h ago · run: git pull
func RepoFreshness(root string) string {
	if root == "" {
		return ""
	}
	git := func(args ...string) (string, bool) {
		c := exec.Command("git", append([]string{"-C", root}, args...)...)
		c.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		out, err := c.Output()
		if err != nil {
			return "", false
		}
		return strings.TrimSpace(string(out)), true
	}
	upstream, ok := git("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	if !ok || upstream == "" {
		return ""
	}
	counts, ok := git("rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if !ok {
		return ""
	}
	var ahead, behind int
	fmt.Sscanf(counts, "%d\t%d", &ahead, &behind)
	fetched := "never"
	if gitDir, ok := git("rev-parse", "--git-dir"); ok {
		if !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		if st, err := os.Stat(filepath.Join(gitDir, "FETCH_HEAD")); err == nil {
			fetched = ago(time.Since(st.ModTime())) + " ago"
		}
	}
	line := fmt.Sprintf("%s +%d/-%d · last fetch %s", upstream, ahead, behind, fetched)
	switch {
	case behind > 0:
		line += " · run: git pull"
	case fetched == "never":
		line += " · run: git fetch"
	}
	return line
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "<1m"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
