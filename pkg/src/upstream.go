package src

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

func upstreamPath(home string) string { return filepath.Join(SrcDir(home), "upstreams.json") }
func configuredUpstreams(home string) (map[string]string, error) {
	m := map[string]string{}
	b, err := os.ReadFile(upstreamPath(home))
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("src: corrupt upstreams: %w", err)
	}
	return m, nil
}
func ConfiguredUpstream(home, repo string) (string, error) {
	if _, err := remoteFor(repo); err != nil {
		return "", err
	}
	m, err := configuredUpstreams(home)
	if err != nil {
		return "", err
	}
	return m[repo], nil
}
func SetUpstream(home, repo, origin string) error {
	if _, err := remoteFor(repo); err != nil {
		return err
	}
	origin = strings.TrimSpace(origin)
	if origin == "" {
		return fmt.Errorf("src: upstream URL or path is required")
	}
	if !strings.Contains(origin, "://") && !strings.HasPrefix(origin, "git@") {
		p, err := filepath.Abs(origin)
		if err != nil {
			return err
		}
		origin = p
	}
	m, err := configuredUpstreams(home)
	if err != nil {
		return err
	}
	m[repo] = origin
	if err = os.MkdirAll(SrcDir(home), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := upstreamPath(home) + ".tmp"
	if err = os.WriteFile(tmp, append(b, '\n'), 0600); err != nil {
		return err
	}
	return os.Rename(tmp, upstreamPath(home))
}
func originFor(home, repo string) (string, error) {
	u, err := ConfiguredUpstream(home, repo)
	if err != nil {
		return "", err
	}
	if u != "" {
		return u, nil
	}
	if s := seedOrigin(home, repo); s != "" {
		return s, nil
	}
	return remoteFor(repo)
}
func newUpstreamCmd() *cobra.Command {
	c := &cobra.Command{Use: "upstream", Short: "Configure this installation's source upstream"}
	c.AddCommand(&cobra.Command{Use: "set <repo> <url-or-path>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		if err := SetUpstream("", a[0], a[1]); err != nil {
			return err
		}
		fmt.Fprintf(cmd.OutOrStdout(), "upstream %s: %s\n", a[0], a[1])
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "show [repo]", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		names := AllowedRepos()
		if len(a) > 0 {
			names = []string{a[0]}
		}
		for _, n := range names {
			u, err := ConfiguredUpstream("", n)
			if err != nil {
				return err
			}
			if u == "" {
				u = "(not configured)"
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", n, u)
		}
		return nil
	}})
	return c
}
