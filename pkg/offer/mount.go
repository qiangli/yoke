package offer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/atlas"
	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/spf13/cobra"
)

// A mounted offer is an ordinary registered command record (no new record
// kind): exec is `bashy remote call HOST NAME {args}`, args is the offered
// schema, and the pin lives in the record's env entries. The env doubles as
// the transport of the pin: the child `remote call` reads it back and sends
// it to the door, so a changed definition refuses instead of running.
const (
	mountEnvHost   = "BASHY_OFFER_HOST"
	mountEnvName   = "BASHY_OFFER_NAME"
	mountEnvDigest = "BASHY_OFFER_DIGEST"
	mountEnvClass  = "BASHY_OFFER_CLASS"
	mountEnvUnit   = "BASHY_OFFER_UNIT"
	mountEnvTags   = "BASHY_OFFER_TAGS"
)

// Mount is one mounted offer as `remote list` reports it.
type Mount struct {
	Name   string   `json:"name"`
	Host   string   `json:"host"`
	Offer  string   `json:"offer"`
	Class  string   `json:"class"`
	Unit   string   `json:"unit"`
	Digest string   `json:"digest"`
	Tags   []string `json:"tags,omitempty"`
}

// mountOf reads the pin back from a registered command; ok is false for a
// record that is not a mounted offer.
func mountOf(c fleet.Command) (Mount, bool) {
	env := map[string]string{}
	for _, kv := range c.Env {
		if k, v, found := strings.Cut(kv, "="); found {
			env[k] = v
		}
	}
	m := Mount{Name: c.Name, Host: env[mountEnvHost], Offer: env[mountEnvName], Class: env[mountEnvClass], Unit: env[mountEnvUnit], Digest: env[mountEnvDigest]}
	if m.Host == "" || m.Offer == "" || m.Digest == "" || len(c.Exec) < 5 || c.Exec[1] != "remote" || c.Exec[2] != "call" || c.Exec[3] != m.Host || c.Exec[4] != m.Offer {
		return Mount{}, false
	}
	if tags := env[mountEnvTags]; tags != "" {
		m.Tags = strings.Split(tags, ",")
	}
	return m, true
}

// pinnedDigest is the digest a mounted record pinned for this host and offer,
// or "" for a direct `remote call`. The pin binds to both names so it cannot
// leak onto another offer called from the same environment.
func pinnedDigest(host, name string) string {
	if os.Getenv(mountEnvHost) != host || os.Getenv(mountEnvName) != name {
		return ""
	}
	return os.Getenv(mountEnvDigest)
}

func digestChanged(host, name, pinned, current string) error {
	return fmt.Errorf("offer %s on %s changed since it was mounted (pinned %s, now %s); run `bashy remote add %s %s` to accept the new definition", name, host, pinned, current, host, name)
}

// fetchDefinition reads the typed definition and its digest over the same
// transport as `remote call` (peer door or satellite member route).
func fetchDefinition(ctx context.Context, host, name string) (ToolDefinition, string, error) {
	base, credential, err := remoteEndpoint(ctx, host, name)
	if err != nil {
		return ToolDefinition{}, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"?definition", nil)
	if err != nil {
		return ToolDefinition{}, "", err
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	client := &http.Client{Timeout: time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return ToolDefinition{}, "", err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ToolDefinition{}, "", err
	}
	if resp.StatusCode >= 300 {
		return ToolDefinition{}, "", fmt.Errorf("remote offer: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var meta struct {
		Definition ToolDefinition `json:"definition"`
		Digest     string         `json:"digest"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return ToolDefinition{}, "", err
	}
	if meta.Definition.Name != name || meta.Digest == "" {
		return ToolDefinition{}, "", fmt.Errorf("remote offer: %s returned no definition for %s", host, name)
	}
	return meta.Definition, meta.Digest, nil
}

// mountSchema turns the offered JSON input schema back into the record's
// argument schema. Every property is a flag: `remote call` takes --ARG value.
func mountSchema(d ToolDefinition) (*fleet.CommandSchema, error) {
	props, _ := d.Input["properties"].(map[string]any)
	var required []string
	switch r := d.Input["required"].(type) {
	case []any:
		for _, v := range r {
			if s, ok := v.(string); ok {
				required = append(required, s)
			}
		}
	case []string:
		required = r
	}
	types := map[string]string{"string": fleet.ArgTypeString, "integer": fleet.ArgTypeInt, "number": fleet.ArgTypeFloat, "boolean": fleet.ArgTypeBool}
	schema := &fleet.CommandSchema{}
	for _, key := range slices.Sorted(func(yield func(string) bool) {
		for k := range props {
			if !yield(k) {
				return
			}
		}
	}) {
		prop, ok := props[key].(map[string]any)
		typ, _ := prop["type"].(string)
		if !ok || types[typ] == "" {
			return nil, fmt.Errorf("offer %s: argument %q has no mountable type", d.Name, key)
		}
		f := fleet.CommandFlag{Name: key, Type: types[typ]}
		literal := func(v any) (string, error) {
			switch x := v.(type) {
			case string:
				return x, nil
			case bool:
				return strconv.FormatBool(x), nil
			case float64:
				if typ == "integer" {
					return strconv.FormatInt(int64(x), 10), nil
				}
				return strconv.FormatFloat(x, 'g', -1, 64), nil
			case int:
				return strconv.Itoa(x), nil
			case int64:
				return strconv.FormatInt(x, 10), nil
			}
			return "", fmt.Errorf("offer %s: argument %q has an unsupported literal", d.Name, key)
		}
		if def, ok := prop["default"]; ok {
			text, err := literal(def)
			if err != nil {
				return nil, err
			}
			f.Default = text
		}
		var enum []any
		switch e := prop["enum"].(type) {
		case []any:
			enum = e
		case []string:
			for _, s := range e {
				enum = append(enum, s)
			}
		}
		for _, e := range enum {
			text, err := literal(e)
			if err != nil {
				return nil, err
			}
			f.Enum = append(f.Enum, text)
		}
		// A required argument may not carry a default in a record.
		f.Required = slices.Contains(required, key) && f.Default == ""
		schema.Flags = append(schema.Flags, f)
	}
	return schema, nil
}

// mountRecord builds the registered command for one offered definition.
func mountRecord(local, host string, d ToolDefinition, digest string) (fleet.Command, error) {
	schema, err := mountSchema(d)
	if err != nil {
		return fleet.Command{}, err
	}
	// The local call reaches the door over the network whatever the offered
	// command does on the far side, so net joins the offered effects.
	effects := append(slices.Clone(d.Effects), atlas.EffNet)
	slices.Sort(effects)
	effects = slices.Compact(effects)
	synopsis, _, _ := strings.Cut(cmp.Or(d.Description, "Run "+d.Name), "\n")
	env := []string{
		mountEnvHost + "=" + host,
		mountEnvName + "=" + d.Name,
		mountEnvDigest + "=" + digest,
		mountEnvClass + "=" + d.Class,
		mountEnvUnit + "=" + d.Unit,
	}
	if len(d.Tags) > 0 {
		env = append(env, mountEnvTags+"="+strings.Join(d.Tags, ","))
	}
	return fleet.Command{
		Name:     local,
		Kind:     fleet.KindCommand,
		Synopsis: synopsis,
		Long:     fmt.Sprintf("Mounted %s offer %s on %s (unit %s, digest %s). Re-pin with: bashy remote add %s %s", d.Class, d.Name, host, d.Unit, digest, host, d.Name),
		Exec:     []string{"bashy", "remote", "call", host, d.Name, fleet.ArgsToken},
		Args:     schema,
		Effects:  effects,
		Env:      env,
	}, nil
}

func newRemoteAddCmd(opts []fleet.Option) *cobra.Command {
	var as string
	var asJSON bool
	c := &cobra.Command{Use: "add HOST NAME", Short: "mount a remote typed offer as a local typed tool", Args: cobra.ExactArgs(2), SilenceUsage: true, RunE: func(c *cobra.Command, argv []string) error {
		host, name := argv[0], argv[1]
		if !broker.ValidPeerName(host) || !broker.ValidPeerName(name) {
			return errors.New("invalid remote host or offer name")
		}
		local := cmp.Or(as, name)
		cat := fleet.New(opts...)
		if existing, ok := cat.Command(local); ok {
			// Re-adding the same mount is the fix for a changed digest.
			if m, mounted := mountOf(existing); !mounted || existing.Name != local || m.Host != host || m.Offer != name {
				return fmt.Errorf("remote add: %q is already a registered command; choose another local name with --as LOCAL", local)
			}
		}
		d, digest, err := fetchDefinition(c.Context(), host, name)
		if err != nil {
			return err
		}
		rec, err := mountRecord(local, host, d, digest)
		if err != nil {
			return err
		}
		if err := cat.SaveCommand(rec); err != nil {
			return fmt.Errorf("remote add: %w; a different local name can be given with --as LOCAL", err)
		}
		m, _ := mountOf(rec)
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(m)
		}
		fmt.Fprintf(c.OutOrStdout(), "mounted %s = %s on %s (%s, %s, %s)\n", m.Name, m.Offer, m.Host, m.Class, m.Unit, m.Digest)
		return nil
	}}
	c.Flags().StringVar(&as, "as", "", "local tool name (default NAME)")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable result")
	return c
}

func newRemoteRemoveCmd(opts []fleet.Option) *cobra.Command {
	var asJSON bool
	c := &cobra.Command{Use: "remove NAME", Short: "unmount a mounted offer", Args: cobra.ExactArgs(1), SilenceUsage: true, RunE: func(c *cobra.Command, argv []string) error {
		cat := fleet.New(opts...)
		rec, ok := cat.Command(argv[0])
		m, mounted := mountOf(rec)
		if !ok || !mounted || rec.Name != argv[0] {
			return fmt.Errorf("remote remove: %q is not a mounted offer", argv[0])
		}
		if err := cat.RemoveCommand(rec.Name); err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"removed": m})
		}
		fmt.Fprintf(c.OutOrStdout(), "removed %s\n", m.Name)
		return nil
	}}
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable result")
	return c
}

func newRemoteListCmd(opts []fleet.Option) *cobra.Command {
	var tag string
	var asJSON bool
	c := &cobra.Command{Use: "list", Short: "list mounted offers", Args: cobra.NoArgs, SilenceUsage: true, RunE: func(c *cobra.Command, _ []string) error {
		cmds, _ := fleet.New(opts...).Commands()
		rows := []Mount{}
		for _, rec := range cmds {
			if m, ok := mountOf(rec); ok && (tag == "" || slices.Contains(m.Tags, tag)) {
				rows = append(rows, m)
			}
		}
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(rows)
		}
		for _, m := range rows {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s\t%s\t%s\t%s\n", m.Name, m.Host, m.Class, m.Unit, m.Digest, strings.Join(m.Tags, ","))
		}
		return nil
	}}
	c.Flags().StringVar(&tag, "tag", "", "only mounts carrying this tag")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable listing")
	return c
}
