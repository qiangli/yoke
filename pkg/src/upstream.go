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
func configuredUpstreams(home string) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
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
	var tiers map[string]string
	if len(m[repo]) == 0 {
		return "", nil
	}
	if m[repo][0] == '"' {
		var u string
		err = json.Unmarshal(m[repo], &u)
		return u, err
	}
	err = json.Unmarshal(m[repo], &tiers)
	return tiers["upstream"], err
}

// ConfiguredTier returns a named promotion destination. The upstream tier
// falls back to the installation seed or canonical origin.
func ConfiguredTier(home, repo, tier string) (string, error) {
	if tier != "org" && tier != "upstream" {
		return "", fmt.Errorf("src: unknown tier %q", tier)
	}
	if tier == "upstream" {
		u, err := ConfiguredUpstream(home, repo)
		if err != nil || u != "" {
			return u, err
		}
		if u = seedOrigin(home, repo); u != "" {
			return u, nil
		}
		return remoteFor(repo)
	}
	if _, err := remoteFor(repo); err != nil {
		return "", err
	}
	m, err := configuredUpstreams(home)
	if err != nil {
		return "", err
	}
	var tiers map[string]string
	if len(m[repo]) == 0 || m[repo][0] == '"' {
		return "", nil
	}
	err = json.Unmarshal(m[repo], &tiers)
	return tiers[tier], err
}
func SetUpstream(home, repo, origin string) error { return SetTier(home, repo, "upstream", origin) }
func SetTier(home, repo, tier, origin string) error {
	if _, err := remoteFor(repo); err != nil {
		return err
	}
	if tier != "org" && tier != "upstream" {
		return fmt.Errorf("src: unknown tier %q", tier)
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
	tiers := map[string]string{}
	var originLegacy string
	if len(m[repo]) > 0 {
		if m[repo][0] == '"' {
			if err = json.Unmarshal(m[repo], &originLegacy); err != nil {
				return err
			}
			tiers["upstream"] = originLegacy
		} else if err = json.Unmarshal(m[repo], &tiers); err != nil {
			return err
		}
	}
	tiers[tier] = origin
	if len(tiers) == 1 && tiers["org"] == "" {
		m[repo], err = json.Marshal(tiers["upstream"])
	} else {
		m[repo], err = json.Marshal(tiers)
	}
	if err != nil {
		return err
	}
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
