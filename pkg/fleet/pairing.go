package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/qiangli/yoke/pkg/outpostagent"
	"github.com/spf13/cobra"
)

// Pairing target vs coordination base.
//
// A host pairs ONCE with the cloudbox portal (agent.json `cloudbox`, identity
// only) and reaches satellites as attachments of its account (agent.json
// `attachments`, docs/cloudbox-members-attach-wire.md). The coordination
// calls (cross-host mb, inbox, sprint, the fleet registry) are served by a
// satellite, so their default base is the attached satellite; account reads
// default to the portal. Either way a credential is only sent to the server
// that issued it: the attachment token to its satellite, the cloudbox host
// token to the portal.
//
// Pre-migration hosts whose top-level pairing points at a satellite keep
// working: the paired token goes only to that server, and the read-only
// legacy ~/.config/bashy/master-token answers for the base otherwise.

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

// outpostAgent is the subset of agent.json this package reads: the legacy
// top-level (tunnel) pairing, the canonical cloudbox identity and the
// satellite attachments.
type outpostAgent struct {
	ServerAddr  string                    `json:"server_addr"`
	PortalURL   string                    `json:"portal_url"`
	AccessToken string                    `json:"access_token"`
	Cloudbox    *outpostagent.Cloudbox    `json:"cloudbox,omitempty"`
	Attachments []outpostagent.Attachment `json:"attachments,omitempty"`
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

// pairingRecord is the first readable agent.json in OutpostAgentFiles order.
func pairingRecord() (outpostAgent, bool) {
	for _, p := range OutpostAgentFiles() {
		if a, ok := readOutpostAgent(p); ok {
			return a, true
		}
	}
	return outpostAgent{}, false
}

// PortalBase is the cloudbox portal this host is paired with (agent.json
// `cloudbox.portal_url`), or "" when it holds no cloudbox identity. It is the
// default base for account reads.
func PortalBase() string {
	a, ok := pairingRecord()
	if !ok || !a.Cloudbox.Paired() {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(a.Cloudbox.PortalURL), "/")
}

// CoordinationBase is the server the coordination verbs default to: the
// first attached satellite's endpoint (one attachment per host is the
// current server limit; the list is read in config order), else the portal,
// else "" (no canonical config — the legacy default applies).
func CoordinationBase() string {
	a, ok := pairingRecord()
	if !ok {
		return ""
	}
	for _, at := range a.Attachments {
		if ep := strings.TrimRight(strings.TrimSpace(at.Endpoint), "/"); ep != "" && strings.TrimSpace(at.Token) != "" {
			return ep
		}
	}
	return PortalBase()
}

// Attachments is the satellite attachments of this host's pairing record
// (agent.json `attachments`), in config order; nil when there is none.
func Attachments() []outpostagent.Attachment {
	a, ok := pairingRecord()
	if !ok {
		return nil
	}
	return a.Attachments
}

// PairedServer is the canonical portal URL the Outpost pairing points at. For
// legacy agent.json files it returns server_addr as before. It returns "" when
// this machine has no readable pairing record (unknown, not "unpaired").
func PairedServer() string {
	for _, p := range OutpostAgentFiles() {
		if a, ok := readOutpostAgent(p); ok {
			return outpostagent.PortalURL(a.PortalURL, a.ServerAddr)
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

// pairedToken is the paired-host rung of ResolveCloud: the credential the
// server at base issued to this host, else the legacy master credential.
//
//  1. an attachment whose endpoint is base → its satellite host token;
//  2. the cloudbox pairing when its portal is base → the host token;
//  3. the legacy top-level pairing when it points at base (or its target is
//     unknown — the pre-migration behavior) → the Outpost token;
//  4. otherwise the read-only legacy master-token file, else "".
func pairedToken(base string) string {
	a, ok := pairingRecord()
	if !ok {
		return outpostToken()
	}
	origin := outpostagent.Origin(base)
	for _, at := range a.Attachments {
		if tok := strings.TrimSpace(at.Token); tok != "" && origin != "" && outpostagent.Origin(at.Endpoint) == origin {
			return tok
		}
	}
	if a.Cloudbox.Paired() && origin != "" && outpostagent.Origin(a.Cloudbox.PortalURL) == origin {
		return strings.TrimSpace(a.Cloudbox.Token)
	}
	if srv := outpostagent.PortalURL(a.PortalURL, a.ServerAddr); srv != "" {
		if hostOf(srv) == hostOf(base) {
			return outpostToken()
		}
		return masterToken(base)
	}
	if a.Cloudbox.Paired() || len(a.Attachments) > 0 {
		// Canonical config, but nothing here was issued by base.
		return masterToken(base)
	}
	return outpostToken()
}

// writeMasterTokenFile stores a coordination-base credential at
// MasterTokenPath (0600), labeled with the base URL it belongs to (line
// 2), so the #599 ladder can tell it apart from other bases' labels.
// The value is written, never returned or printed.
func writeMasterTokenFile(tokenValue, base string) (string, error) {
	path := MasterTokenPath()
	if path == "" {
		return "", fmt.Errorf("fleet: no config directory for the master token")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	body := strings.TrimSpace(tokenValue) + "\n"
	if h := hostOf(base); h != "" {
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
	return writeMasterTokenFile(a.AccessToken, a.ServerAddr)
}

// masterTokenDeprecation is printed (stderr) by every master-token writer. A
// host pairs once with the cloudbox portal and reaches satellites as
// attachments, so the coordination base needs no saved master credential;
// the file is still READ as a legacy fallback for hosts not yet migrated.
const masterTokenDeprecation = "deprecated: a host pairs once with cloudbox (`outpost register --code CODE`) and reaches a satellite as an attachment (`outpost satellite attach ID`); the master-token file is read only as a legacy fallback and will be removed"

func warnMasterTokenDeprecated(cmd *cobra.Command) {
	fmt.Fprintf(cmd.ErrOrStderr(), "bashy fleet %s: %s\n", cmd.Name(), masterTokenDeprecation)
}

// NewMasterTokenCmd is `master-token save --from <agent.json>`, the writer a
// pairing migration ran BEFORE re-pairing to a satellite. Deprecated (S606):
// pair once with cloudbox and attach instead; both writers still work and
// print the deprecation. Hosts mount it under their fleet verb (`bashy fleet
// master-token`).
func NewMasterTokenCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "master-token",
		Short: "(deprecated) Manage the saved legacy coordination credential; pair once with cloudbox and attach a satellite instead",
	}
	var from string
	save := &cobra.Command{
		Use:   "save --from <agent.json>",
		Short: "(deprecated) Save the access_token of a pre-migration Outpost agent.json (0600; never printed)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warnMasterTokenDeprecated(cmd)
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
	c.AddCommand(newMasterTokenMintCmd())
	return c
}

// coordTokenScopes is the standing set a migrated host needs for
// fleet coordination: cross-host mb, inbox, the fleet registry. Nothing
// wider — the migration hook must not mint an admin-capable credential.
var coordTokenScopes = []string{"tasks:read", "tasks:write"}

// coordTokenName derives the per-host coordination token name. One token
// per host, revocable individually: the shared manual
// DHNT_FLEET_COORD_TOKEN goes away.
func coordTokenName(host string) (string, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return "", fmt.Errorf("fleet: cannot determine the host name (pass --host-name)")
	}
	var b strings.Builder
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := "coord-" + b.String()
	if len(name) > 64 {
		name = name[:64]
	}
	name = strings.Trim(name, "-.")
	if err := validateTokenName(name); err != nil {
		return "", err
	}
	return name, nil
}

// newMasterTokenMintCmd is `master-token mint`, the S490.5 migration
// hook: moving a host to a satellite mints that host's own coordination
// token from its existing ai.dhnt.io credential and writes the
// master-token file, so cross-host mb keeps working with no console
// step. The minted value is never printed. Re-running replaces the
// file (revoking the previous coord-<host> token it replaced).
func newMasterTokenMintCmd() *cobra.Command {
	var from, hostName, ttl string
	mint := &cobra.Command{
		Use:   "mint",
		Short: "(deprecated) Mint this host's own coordination token and save it as the master credential (0600; never printed)",
		Long: "Mint this host's own coordination token and save it as the master credential.\n\n" +
			"Deprecated: a host pairs once with cloudbox and reaches a satellite as an\n" +
			"attachment, so coordination needs no master credential. The file is\n" +
			"read only as a legacy fallback for hosts not yet migrated.\n\n" +
			"Moving a host to a satellite mints its own tasks:read,tasks:write\n" +
			"coordination token — name coord-<host> — from the host's existing\n" +
			"coordination-base credential, and writes ~/.config/bashy/master-token\n" +
			"(0600, base URL on line 2) exactly like `master-token save`. The\n" +
			"value is never printed. Re-running replaces the file and revokes\n" +
			"the previous coord-<host> token it replaced: one token per host,\n" +
			"revocable individually.",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			warnMasterTokenDeprecated(cmd)
			if err := validateTokenTTL(ttl); err != nil {
				return err
			}
			host := strings.TrimSpace(hostName)
			if host == "" {
				h, err := os.Hostname()
				if err != nil {
					return fmt.Errorf("fleet: cannot determine the host name (pass --host-name): %w", err)
				}
				host = h
			}
			name, err := coordTokenName(host)
			if err != nil {
				return err
			}
			// The caller credential: --from's access_token, or — when the
			// host is still paired to the base — the current pairing via
			// ResolveCloud. The #599 ladder rule is unchanged.
			var base, caller string
			if from != "" {
				a, ok := readOutpostAgent(from)
				if !ok {
					return fmt.Errorf("fleet: cannot read %s as an Outpost agent.json", from)
				}
				if strings.TrimSpace(a.AccessToken) == "" {
					return fmt.Errorf("fleet: %s has no access_token", from)
				}
				// agent.json stores a bare host (e.g. ai.dhnt.io); the client
				// needs a URL. A value that already carries a scheme is kept.
				base = mintBaseURL(a.ServerAddr)
				caller = strings.TrimSpace(a.AccessToken)
			} else {
				base, caller = ResolveMaster("", "")
			}
			if caller == "" {
				return fmt.Errorf("fleet: no coordination-base credential (pass --from <agent.json> or pair to the base first)")
			}
			if strings.TrimSpace(base) == "" {
				base = "https://ai.dhnt.io"
			}
			client := TokenClient{BaseURL: strings.TrimRight(base, "/"), Token: caller, HTTP: httpClient()}
			m, err := client.Create(name, coordTokenScopes, ttl)
			if err != nil {
				// Idempotent re-run: the previous coord-<host> row still
				// holds the name (409), so revoke it and mint again.
				var terr *TokenError
				if !errors.As(err, &terr) || terr.Status != http.StatusConflict {
					return err
				}
				rows, lerr := client.List()
				if lerr != nil {
					return err
				}
				prev := ""
				for _, r := range rows {
					if r.Name == name {
						prev = r.ID
						break
					}
				}
				if prev == "" {
					return err
				}
				if rerr := client.Revoke(prev); rerr != nil {
					return err
				}
				if m, err = client.Create(name, coordTokenScopes, ttl); err != nil {
					return err
				}
			}
			path, err := writeMasterTokenFile(m.Token, base)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "minted %s (%s) saved master token to %s\n", m.Name, m.ID, path)
			return nil
		},
	}
	mint.Flags().StringVar(&from, "from", "", "pre-migration Outpost agent.json to mint from (else the current pairing via ResolveCloud)")
	mint.Flags().StringVar(&hostName, "host-name", "", "host the coord-<host> token is minted for (default os.Hostname)")
	mint.Flags().StringVar(&ttl, "ttl", "365d", "time to live, e.g. 90d (max 365d)")
	return mint
}

// mintBaseURL turns an agent.json server_addr into a client base URL: a bare
// host (optionally with port) becomes https://host; a value with a scheme is
// kept as-is; empty stays empty (the caller falls back to the default base).
func mintBaseURL(serverAddr string) string {
	sa := strings.TrimRight(strings.TrimSpace(serverAddr), "/")
	if sa == "" || strings.Contains(sa, "://") {
		return sa
	}
	return "https://" + sa
}
