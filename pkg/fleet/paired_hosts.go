package fleet

// `host ls` — paired hosts from the master, read-only (S490 story 7107fb14).
//
// It speaks only two Bearer [REDACTED] endpoints, both already served by the
// master (cloudbox hub/internal/server/routes_peer.go):
//
//	GET /api/v1/hosts -> {"hosts": [{host, os_user, title, hostname,
//	                  online, shared, apps, builtins}]}
//	GET /api/v1/peers -> {"peers": [{host, alias, owned, shared, online,
//	                  location, version, commit, os, arch, ...}]}
//
// The rows are joined by host name; the peers half adds the same-LAN
// location hint and the build/OS details the host reports. Base URL and
// Bearer [REDACTED] resolve through ResolveCloud — the ONE ladder every bashy-side
// cloudbox client walks (see cloudbox.go; the same resolution `bashy token`
// uses). Nothing is printed but the records; credentials never are.
//
// --satellite uses the Bearer assignment endpoint to filter both catalogs.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"
)

// HostEntry is one row of GET /api/v1/hosts: the routing identity plus
// the app catalog. Only the fields this verb prints are decoded; --json
// passes the envelope through untouched.
type HostEntry struct {
	Host     string `json:"host"`
	OsUser   string `json:"os_user,omitempty"`
	Title    string `json:"title,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Online   bool   `json:"online"`
	Shared   bool   `json:"shared,omitempty"`
}

// PeerEntry is one row of GET /api/v1/peers: online status, the same-LAN
// location hint, and the build/OS details the host reports.
type PeerEntry struct {
	Host     string `json:"host"`
	Alias    string `json:"alias,omitempty"`
	Owned    bool   `json:"owned"`
	Shared   bool   `json:"shared,omitempty"`
	Online   bool   `json:"online"`
	Location string `json:"location,omitempty"`
	Version  string `json:"version,omitempty"`
	Commit   string `json:"commit,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
}

// PairedHost is the joined view: the hosts row enriched with the peers
// row of the same name.
type PairedHost struct {
	Host     string `json:"host"`
	OsUser   string `json:"os_user,omitempty"`
	Title    string `json:"title,omitempty"`
	Hostname string `json:"hostname,omitempty"`
	Online   bool   `json:"online"`
	Shared   bool   `json:"shared,omitempty"`
	Location string `json:"location,omitempty"`
	Version  string `json:"version,omitempty"`
	OS       string `json:"os,omitempty"`
	Arch     string `json:"arch,omitempty"`
}

// PairedHostsClient talks to the master paired-host surface. Zero network
// state — one *http.Client, a resolved base URL, and a caller token,
// exactly like ProvisioningClient and TokenClient.
type PairedHostsClient struct {
	BaseURL string // e.g. https://ai.dhnt.io (no trailing /api/v1)
	Token   string // Bearer [REDACTED] for the v1 hosts/peers API
	HTTP    *http.Client
}

// NewPairedHostsClient resolves base + caller token through
// ResolveCloud, the same ladder `bashy token` walks.
func NewPairedHostsClient(urlOverride, tokenOverride string) (PairedHostsClient, error) {
	base, tok := ResolveCloud(urlOverride, tokenOverride)
	if tok == "" {
		return PairedHostsClient{}, fmt.Errorf("fleet: no cloudbox token (set $BASHY_FLEET_TOKEN or pass --token)")
	}
	return PairedHostsClient{BaseURL: base, Token: tok, HTTP: httpClient()}, nil
}

func (c PairedHostsClient) get(path string) ([]byte, error) {
	op := http.MethodGet + " " + path
	req, err := http.NewRequest(http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		return nil, provisionAPIError(op, resp.StatusCode, raw)
	}
	return raw, nil
}

// ListHosts returns the paired hosts visible to the caller plus the raw
// envelope for --json passthrough.
func (c PairedHostsClient) ListHosts() ([]HostEntry, json.RawMessage, error) {
	raw, err := c.get("/api/v1/hosts")
	if err != nil {
		return nil, nil, err
	}
	var env struct {
		Hosts []HostEntry `json:"hosts"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("fleet: GET /api/v1/hosts: decode: %w", err)
	}
	if env.Hosts == nil {
		env.Hosts = []HostEntry{}
	}
	return env.Hosts, json.RawMessage(raw), nil
}

// ListPeers returns the peer status board plus the raw envelope for
// --json passthrough.
func (c PairedHostsClient) ListPeers() ([]PeerEntry, json.RawMessage, error) {
	raw, err := c.get("/api/v1/peers")
	if err != nil {
		return nil, nil, err
	}
	var env struct {
		Peers []PeerEntry `json:"peers"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, nil, fmt.Errorf("fleet: GET /api/v1/peers: decode: %w", err)
	}
	if env.Peers == nil {
		env.Peers = []PeerEntry{}
	}
	return env.Peers, json.RawMessage(raw), nil
}

// ListPaired joins the hosts rows with their peers rows by host name.
// A host missing from the peers board keeps its hosts row with empty
// location/version columns rather than vanishing.
func (c PairedHostsClient) ListPaired() ([]PairedHost, error) {
	hosts, _, err := c.ListHosts()
	if err != nil {
		return nil, err
	}
	peers, _, err := c.ListPeers()
	if err != nil {
		return nil, err
	}
	return joinPaired(hosts, peers), nil
}

func joinPaired(hosts []HostEntry, peers []PeerEntry) []PairedHost {
	byName := make(map[string]PeerEntry, len(peers))
	for _, p := range peers {
		byName[p.Host] = p
	}
	out := make([]PairedHost, 0, len(hosts))
	for _, h := range hosts {
		row := PairedHost{
			Host:     h.Host,
			OsUser:   h.OsUser,
			Title:    h.Title,
			Hostname: h.Hostname,
			Online:   h.Online,
			Shared:   h.Shared,
		}
		if p, ok := byName[h.Host]; ok {
			row.Online = h.Online || p.Online
			if !row.Shared {
				row.Shared = p.Shared
			}
			row.Location = p.Location
			row.Version = p.Version
			row.OS = p.OS
			row.Arch = p.Arch
		}
		out = append(out, row)
	}
	return out
}

// NewPairedHostCmd builds the `host` verb tree: the paired hosts the
// master sees. Read-only; the only verb in this run is `ls`.
func NewPairedHostCmd() *cobra.Command {
	var master, token, satellite string
	var acct accountFlag
	var asJSON bool
	c := &cobra.Command{
		Use:   "host",
		Short: "Paired hosts from the master (read-only)",
		Long: "Paired hosts from the master (read-only).\n\n" +
			"Reads GET /api/v1/hosts and GET /api/v1/peers and joins them by\n" +
			"host name. --satellite filters through GET /api/v1/satellites/assignment.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	c.PersistentFlags().StringVar(&master, "master", "", "master base URL (default $BASHY_CLOUDBOX_URL, else https://ai.dhnt.io)")
	c.PersistentFlags().StringVar(&token, "token", "", "master Bearer [REDACTED] (default $BASHY_FLEET_TOKEN, else $BASHY_API_KEY, else the paired credential)")
	c.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")
	acct.bindPersistent(c)

	ls := &cobra.Command{
		Use:   "ls",
		Short: "List the paired hosts visible to the caller",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newPairedHostsClientFor(cmd.Context(), &acct, master, token)
			if err != nil {
				return err
			}
			hosts, _, err := client.ListHosts()
			if err != nil {
				return err
			}
			peers, _, err := client.ListPeers()
			if err != nil {
				return err
			}
			if id := strings.TrimSpace(satellite); id != "" {
				assignments, err := (OwnerClient{BaseURL: client.BaseURL, Token: client.Token, HTTP: client.HTTP}).SatelliteAssignments(cmd.Context(), id)
				if err != nil {
					return err
				}
				allowed := make(map[string]bool, len(assignments))
				for _, a := range assignments {
					if a.SatelliteID == id {
						allowed[a.Host] = true
					}
				}
				filteredHosts := make([]HostEntry, 0)
				for _, h := range hosts {
					if allowed[h.Host] {
						filteredHosts = append(filteredHosts, h)
					}
				}
				filteredPeers := make([]PeerEntry, 0)
				for _, p := range peers {
					if allowed[p.Host] {
						filteredPeers = append(filteredPeers, p)
					}
				}
				hosts, peers = filteredHosts, filteredPeers
			}
			out := cmd.OutOrStdout()
			if asJSON {
				joined, err := json.Marshal(map[string]any{
					"hosts": hosts,
					"peers": peers,
				})
				if err != nil {
					return err
				}
				_, err = out.Write(append(joined, '\n'))
				return err
			}
			rows := joinPaired(hosts, peers)
			if len(rows) == 0 {
				fmt.Fprintln(out, "no hosts")
				return nil
			}
			fmt.Fprintln(out, "HOST\tONLINE\tSHARED\tLOCATION\tOS/ARCH\tVERSION")
			for _, r := range rows {
				osArch := dashIfEmpty(joinSlash(r.OS, r.Arch))
				fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n",
					r.Host, yesNo(r.Online), yesNo(r.Shared),
					dashIfEmpty(r.Location), osArch, dashIfEmpty(r.Version))
			}
			return nil
		},
	}
	ls.Aliases = []string{"list"}
	ls.Flags().StringVar(&satellite, "satellite", "", "filter by satellite ID")
	c.AddCommand(ls)
	return c
}

func joinSlash(a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + "/" + b
}
