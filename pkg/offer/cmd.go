package offer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/toolcmd"
	"github.com/spf13/cobra"
)

var outpostRun = func(ctx context.Context, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "outpost", args...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("outpost %s: %v: %s", strings.Join(args[:min(len(args), 3)], " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// NewOfferCmd builds the owner-facing offer noun.
func NewOfferCmd() *cobra.Command {
	var circles, groups, emails, tags []string
	var limit uint64
	var duration time.Duration
	var tokenCap int64
	var asJSON bool
	cmd := &cobra.Command{Use: "offer NAME", Short: "share a typed model, tool or agent call through a gated app", Args: cobra.ExactArgs(1), SilenceUsage: true}
	cmd.RunE = func(c *cobra.Command, args []string) error {
		name := args[0]
		if !broker.ValidPeerName(name) {
			return fmt.Errorf("invalid offer name %q", name)
		}
		if len(circles)+len(groups)+len(emails) == 0 {
			return errors.New("offer needs --circle, --group or --email")
		}
		if duration < 0 {
			return errors.New("--for must be positive")
		}
		d, err := Offerable(fleet.New(), name)
		if err != nil {
			return err
		}
		if d.Class == "agent" && tokenCap <= 0 {
			return errors.New("agent offer requires --token-cap")
		}
		if d.Class != "agent" && tokenCap != 0 {
			return errors.New("--token-cap applies only to agent offers")
		}
		d = d.WithTags(tags...)
		digest, err := d.Digest()
		if err != nil {
			return err
		}
		definition, err := json.Marshal(d)
		if err != nil {
			return err
		}
		rec := broker.OfferRecord{Name: name, Definition: definition, Digest: digest, Limit: limit, TokenCap: tokenCap}
		rec.Audience.Circles, rec.Audience.Groups, rec.Audience.Emails = circles, groups, emails
		if duration > 0 {
			rec.Expires = time.Now().Add(duration).UTC()
		}
		dir, err := broker.OffersDir()
		if err != nil {
			return err
		}
		if _, err := broker.LoadOffer(dir, name); err == nil {
			return fmt.Errorf("offer %s already exists; stop it before changing its audience", name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		app := "offer-" + name
		if err := ensureApp(c.Context(), app); err != nil {
			return err
		}
		secret, err := outpostRun(c.Context(), "apps", "secret", app)
		if err != nil {
			return err
		}
		rec.SSOSecret = strings.TrimSpace(string(secret))
		if rec.SSOSecret == "" {
			return errors.New("outpost returned no app secret")
		}
		for _, pair := range []struct {
			flag   string
			values []string
		}{{"--circle", circles}, {"--group", groups}, {"--email", emails}} {
			for _, value := range pair.values {
				if _, err := outpostRun(c.Context(), "apps", "share", app, pair.flag, value); err != nil {
					return err
				}
			}
		}
		if err := broker.SaveOffer(dir, rec); err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"name": name, "class": d.Class, "unit": d.Unit, "digest": digest, "audience": rec.Audience, "consumer": "bashy remote call HOST " + name})
		}
		fmt.Fprintf(c.OutOrStdout(), "offered %s (%s, %s); post: bashy remote call HOST %s\n", name, d.Class, d.Unit, name)
		return nil
	}
	cmd.Flags().StringSliceVar(&circles, "circle", nil, "audience circle")
	cmd.Flags().StringSliceVar(&groups, "group", nil, "audience group")
	cmd.Flags().StringSliceVar(&emails, "email", nil, "audience email")
	cmd.Flags().Uint64Var(&limit, "limit", 0, "usage limit in the offer's unit")
	cmd.Flags().DurationVar(&duration, "for", 0, "offer lifetime")
	cmd.Flags().Int64Var(&tokenCap, "token-cap", 0, "agent total token cap")
	cmd.Flags().StringSliceVar(&tags, "tag", nil, "definition tag")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable result")
	cmd.AddCommand(newListCmd(), newStopCmd(), newOfferUsageCmd(false))
	return cmd
}

func ensureApp(ctx context.Context, app string) error {
	out, err := outpostRun(ctx, "apps", "list", "--json")
	if err != nil {
		return err
	}
	var listing struct {
		Apps []struct {
			Name         string `json:"name"`
			Host         string `json:"host"`
			Port         int    `json:"port"`
			BasePath     string `json:"base_path"`
			Enabled      bool   `json:"enabled"`
			RequireLogin bool   `json:"require_login"`
			Trust        bool   `json:"trust_cloud_identity"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(out, &listing); err != nil {
		return err
	}
	for _, a := range listing.Apps {
		if a.Name == app {
			ip := net.ParseIP(a.Host)
			if !a.RequireLogin || !a.Trust || !a.Enabled || a.Port != door.Port() || a.BasePath != "" || !(a.Host == "localhost" || (ip != nil && ip.IsLoopback())) {
				return fmt.Errorf("app %s exists without the gated loopback door target", app)
			}
			return nil
		}
	}
	_, err = outpostRun(ctx, "apps", "add", app, "--url", door.BaseURL(), "--require-login", "--trust-cloud-identity")
	return err
}

func newListCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{Use: "list", Short: "list registered offers", RunE: func(c *cobra.Command, _ []string) error {
		dir, err := broker.OffersDir()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if errors.Is(err, os.ErrNotExist) {
			entries = nil
		} else if err != nil {
			return err
		}
		rows := make([]map[string]any, 0)
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			r, err := broker.LoadOffer(dir, strings.TrimSuffix(e.Name(), ".json"))
			if err != nil {
				return err
			}
			var d ToolDefinition
			if err := json.Unmarshal(r.Definition, &d); err != nil {
				return err
			}
			rows = append(rows, map[string]any{"name": r.Name, "class": d.Class, "unit": d.Unit, "digest": r.Digest, "audience": r.Audience})
		}
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(rows)
		}
		for _, row := range rows {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\n", row["name"], row["class"], row["unit"], row["digest"])
		}
		return nil
	}}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable listing")
	return c
}

func newStopCmd() *cobra.Command {
	return &cobra.Command{Use: "stop NAME", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		dir, err := broker.OffersDir()
		if err != nil {
			return err
		}
		if _, err := broker.LoadOffer(dir, args[0]); err != nil {
			return err
		}
		if _, err := outpostRun(c.Context(), "apps", "rm", "offer-"+args[0]); err != nil {
			return err
		}
		return os.Remove(filepath.Join(dir, args[0]+".json"))
	}}
}

// RemoteSubcommands are mounted under bashy's existing remote noun.
// The options configure the catalog a mount is written to (the embedding
// shell passes its reserved names so a mount cannot shadow a shipped verb).
func RemoteSubcommands(opts ...fleet.Option) []*cobra.Command {
	return []*cobra.Command{newRemoteCallCmd(), newRemoteAddCmd(opts), newRemoteRemoveCmd(opts), newRemoteListCmd(opts), newOfferUsageCmd(true, opts...)}
}

func newRemoteCallCmd() *cobra.Command {
	c := &cobra.Command{Use: "call HOST NAME [--ARG value ...]", Short: "call a remote typed offer", Args: cobra.MinimumNArgs(2), DisableFlagParsing: true, RunE: func(c *cobra.Command, argv []string) error {
		host, name := argv[0], argv[1]
		if !broker.ValidPeerName(host) || !broker.ValidPeerName(name) {
			return errors.New("invalid remote host or offer name")
		}
		var definition bool
		var flags []string
		for _, arg := range argv[2:] {
			if arg == "--json" {
				continue
			} else if arg == "--definition" {
				definition = true
			} else {
				flags = append(flags, arg)
			}
		}
		base, credential, err := remoteEndpoint(c.Context(), host, name)
		if err != nil {
			return err
		}
		// Set when this call runs through a mounted record (mount.go).
		pinned := pinnedDigest(host, name)
		client := &http.Client{Timeout: 30 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request := func(method, suffix string, body []byte) ([]byte, error) {
			req, err := http.NewRequestWithContext(c.Context(), method, base+suffix, bytes.NewReader(body))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Authorization", "Bearer "+credential)
			req.Header.Set("Content-Type", "application/json")
			if pinned != "" {
				req.Header.Set(broker.OfferDigestHeader, pinned)
			}
			resp, err := client.Do(req)
			if err != nil {
				return nil, err
			}
			defer resp.Body.Close()
			if method == http.MethodPost && resp.StatusCode/100 == 2 {
				return nil, copyRemoteResult(c.OutOrStdout(), host, resp)
			}
			data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			if err != nil {
				return nil, err
			}
			if resp.StatusCode >= 300 {
				return nil, fmt.Errorf("remote offer: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
			}
			return data, nil
		}
		data, err := request("GET", "?definition", nil)
		if err != nil {
			return err
		}
		if err := rememberRemoteDefinition(data); err != nil {
			return err
		}
		if definition {
			_, err = c.OutOrStdout().Write(data)
			return err
		}
		var meta struct {
			Definition ToolDefinition `json:"definition"`
			Digest     string         `json:"digest"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return err
		}
		if pinned != "" && meta.Digest != pinned {
			return digestChanged(host, name, pinned, meta.Digest)
		}
		args, err := parseRemoteArgs(meta.Definition, flags)
		if err != nil {
			return err
		}
		body, _ := json.Marshal(map[string]any{"args": args})
		_, err = request("POST", "", body)
		return err
	}}
	return c
}

func parseRemoteArgs(d ToolDefinition, flags []string) (map[string]any, error) {
	props, _ := d.Input["properties"].(map[string]any)
	args := map[string]any{}
	for i := 0; i < len(flags); i++ {
		if !strings.HasPrefix(flags[i], "--") {
			return nil, errors.New("remote call arguments must be --ARG value pairs")
		}
		// A typed tool call spells a true boolean as a bare --ARG and may
		// spell any value as --ARG=value; both bind like --ARG value.
		key, text, inline := strings.Cut(strings.TrimPrefix(flags[i], "--"), "=")
		prop, ok := props[key].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unknown argument %q", key)
		}
		switch {
		case inline:
		case prop["type"] == "boolean" && (i+1 >= len(flags) || strings.HasPrefix(flags[i+1], "--")):
			text = "true"
		case i+1 >= len(flags):
			return nil, errors.New("remote call arguments must be --ARG value pairs")
		default:
			i++
			text = flags[i]
		}
		var val any = text
		switch prop["type"] {
		case "integer":
			n, err := strconv.ParseInt(text, 10, 64)
			if err != nil {
				return nil, err
			}
			val = n
		case "number":
			n, err := strconv.ParseFloat(text, 64)
			if err != nil {
				return nil, err
			}
			val = n
		case "boolean":
			b, err := strconv.ParseBool(text)
			if err != nil {
				return nil, err
			}
			val = b
		}
		args[key] = val
	}
	if err := validateInput(d, args); err != nil {
		return nil, err
	}
	return args, nil
}

func remoteEndpoint(ctx context.Context, host, name string) (string, string, error) {
	peer, peerErr := toolcmd.LoadPeer(host)
	if peerErr == nil {
		return strings.TrimRight(peer.URL, "/") + "/offers/" + name, peer.Credential, nil
	}
	if !errors.Is(peerErr, toolcmd.ErrUnknownPeer) {
		return "", "", peerErr
	}
	// The satellite endpoint is explicit to avoid sending the account token
	// to a guessed origin. An attachment may provide this URL in a later mount.
	base := strings.TrimSpace(os.Getenv("BASHY_SATELLITE_URL"))
	if base == "" {
		base = fleet.CoordinationBase()
	}
	if base == "" {
		return "", "", errors.New("remote call needs a toolcmd peer or BASHY_SATELLITE_URL")
	}
	url := strings.TrimRight(base, "/") + "/matrix/h/" + host + "/app/offer-" + name + "/offers/" + name
	parsed, err := fleet.ParseSatelliteAppBase(url)
	if err != nil {
		return "", "", err
	}
	account := strings.TrimSpace(os.Getenv("BASHY_FLEET_TOKEN"))
	if account == "" {
		return "", "", errors.New("remote call needs BASHY_FLEET_TOKEN")
	}
	source := fleet.SatelliteTokenSource{Client: fleet.SatelliteClient{BaseURL: parsed.Satellite, Token: account}}
	token, err := source.Token(ctx)
	return parsed.URL, token, err
}
