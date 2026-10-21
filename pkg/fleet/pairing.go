package fleet

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"
)

// Pairing target vs coordination base.
//
// A host's Outpost pairing can move from the master control plane to a
// satellite, but bashy's coordination calls (cross-host mb, inbox, the fleet
// registry) still go to the coordination base. The pairing token then belongs
// to a DIFFERENT server, and sending it there is both a 401 and a credential
// leaked to the wrong host. So the paired token is only offered when the
// pairing points at the base; otherwise a saved master credential is used,
// or nothing.

// configHome is $XDG_CONFIG_HOME, else <home>/.config (USERPROFILE on Windows
// via os.UserHomeDir).
func configHome() string {
	if d := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config")
}

// OutpostAgentFiles lists where the Outpost daemon's agent.json may live, in
// precedence order: outpost's own dir, then the pre-rename `matrix` dir, each
// under $XDG_CONFIG_HOME first and ~/.config second.
func OutpostAgentFiles() []string {
	var roots []string
	if d := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); d != "" {
		roots = append(roots, d)
	}
	if home, err := os.UserHomeDir(); err == nil {
		if d := filepath.Join(home, ".config"); len(roots) == 0 || filepath.Clean(roots[0]) != d {
			roots = append(roots, d) // skip the duplicate when XDG is the default
		}
	}
	var out []string
	for _, dir := range []string{"outpost", "matrix"} {
		for _, r := range roots {
			out = append(out, filepath.Join(r, dir, "agent.json"))
		}
	}
	return out
}

// outpostAgent is the subset of agent.json this package reads.
type outpostAgent struct {
	ServerAddr  string `json:"server_addr"`
	AccessToken string `json:"access_token"`
}

func readOutpostAgent(path string) (outpostAgent, bool) {
	var a outpostAgent
	data, err := os.ReadFile(path)
	if err != nil {
		return a, false
	}
	if json.Unmarshal(data, &a) != nil {
		return outpostAgent{}, false
	}
	return a, true
}

// PairedServer is the host the Outpost pairing points at, or "" when this
// machine has no readable pairing record (unknown, not "unpaired").
func PairedServer() string {
	for _, p := range OutpostAgentFiles() {
		if a, ok := readOutpostAgent(p); ok {
			return a.ServerAddr
		}
	}
	return ""
}

// hostOf normalizes a URL or host[:port] to a lowercase hostname.
func hostOf(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
}

// MasterTokenPath is the host's saved coordination-base credential:
// $XDG_CONFIG_HOME/bashy/master-token (else ~/.config/bashy/master-token).
// Deliberately independent of $BASHY_HOME — it is per host account.
func MasterTokenPath() string {
	ch := configHome()
	if ch == "" {
		return ""
	}
	return filepath.Join(ch, "bashy", "master-token")
}

// masterToken reads the saved credential for base. Line 1 is the token; an
// optional line 2 names the base URL it belongs to, and a label naming a
// different host disqualifies it.
func masterToken(base string) string {
	p := MasterTokenPath()
	if p == "" {
		return ""
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	tok := strings.TrimSpace(lines[0])
	if len(lines) > 1 {
		if label := strings.TrimSpace(lines[1]); label != "" && hostOf(label) != hostOf(base) {
			return ""
		}
	}
	return tok
}

// pairedToken is the paired-host rung of ResolveCloud: the Outpost token when
// the pairing points at base (or its target is unknown — the pre-migration
// behavior), else the master credential, else "".
func pairedToken(base string) string {
	if srv := PairedServer(); srv != "" && hostOf(srv) != hostOf(base) {
		return masterToken(base)
	}
	return outpostToken()
}

// SaveMasterToken copies the access_token out of a pre-migration agent.json
// into MasterTokenPath (0600), labeled with that file's server. The token is
// never returned or printed.
func SaveMasterToken(fromAgentJSON string) (string, error) {
	a, ok := readOutpostAgent(fromAgentJSON)
	if !ok {
		return "", fmt.Errorf("fleet: cannot read %s as an Outpost agent.json", fromAgentJSON)
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return "", fmt.Errorf("fleet: %s has no access_token", fromAgentJSON)
	}
	path := MasterTokenPath()
	if path == "" {
		return "", fmt.Errorf("fleet: no config directory for the master token")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	body := strings.TrimSpace(a.AccessToken) + "\n"
	if h := hostOf(a.ServerAddr); h != "" {
		body += "https://" + h + "\n"
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".master-token-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil && runtime.GOOS != "windows" {
		tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", err
	}
	return path, nil
}

// NewMasterTokenCmd is `master-token save --from <agent.json>`, the writer a
// pairing migration runs BEFORE re-pairing to a satellite. Hosts mount it
// under their fleet verb (`bashy fleet master-token`).
func NewMasterTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "master-token",
		Short: "Manage the saved coordination-base credential used after a satellite migration",
	}
	var from string
	save := &cobra.Command{
		Use:   "save --from <agent.json>",
		Short: "Save the access_token of a pre-migration Outpost agent.json (0600; never printed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if from == "" {
				return fmt.Errorf("--from is required")
			}
			path, err := SaveMasterToken(from)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "saved master token to %s\n", path)
			return nil
		},
	}
	save.Flags().StringVar(&from, "from", "", "pre-migration Outpost agent.json to read access_token from")
	c.AddCommand(save)
	return c
}
