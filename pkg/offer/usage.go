package offer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/qiangli/yoke/pkg/broker"
	"github.com/qiangli/yoke/pkg/broker/door"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/sharedfiles"
	"github.com/spf13/cobra"
)

func init() { sharedfiles.RecordUsage = broker.RecordRemoteResponse }

// Current doors expose the same measurement in the envelope and header.
// The header lets large results pass straight through to the caller.
func copyRemoteResult(out io.Writer, host string, resp *http.Response) error {
	if resp.Header.Get(broker.OfferUsageHeader) != "" {
		if err := broker.RecordRemoteResponse(host, resp); err != nil {
			return fmt.Errorf("record completed remote call: %w", err)
		}
		_, err := io.Copy(out, resp.Body)
		return err
	}
	var envelope struct {
		Usage broker.OfferUsage `json:"usage"`
	}
	if err := json.NewDecoder(io.TeeReader(resp.Body, out)).Decode(&envelope); err != nil {
		return err
	}
	// A K4 door returns an untyped usage object.
	if envelope.Usage.Tool == "" {
		return nil
	}
	return broker.AppendRemoteUsage(host, envelope.Usage)
}

func rememberRemoteDefinition(body []byte) error {
	var meta struct {
		Definition json.RawMessage `json:"definition"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		return err
	}
	if len(meta.Definition) == 0 {
		return errors.New("missing offer definition")
	}
	path, err := broker.RemoteUsagePath()
	if err != nil {
		return err
	}
	return broker.RememberOfferDefinition(filepath.Join(filepath.Dir(path), "remote-offers"), meta.Definition)
}

func usageDigests(dir, tag string) (map[string]bool, error) {
	matches := map[string]bool{}
	paths, err := filepath.Glob(filepath.Join(dir, "definitions", "*.json"))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var def ToolDefinition
		if err = json.Unmarshal(raw, &def); err != nil {
			return nil, err
		}
		if def.HasTag(tag) {
			matches[strings.TrimSuffix(filepath.Base(path), ".json")] = true
		}
	}
	// Offers saved before the definition archive was introduced are still live.
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return matches, nil
	}
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		rec, err := broker.LoadOffer(dir, strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		var def ToolDefinition
		if err = json.Unmarshal(rec.Definition, &def); err != nil {
			return nil, err
		}
		if def.HasTag(tag) {
			matches[rec.Digest] = true
		}
	}
	return matches, nil
}

func newOfferUsageCmd(remote bool, catalogOptions ...fleet.Option) *cobra.Command {
	primary := "member"
	if remote {
		primary = "host"
	}
	var by, tag string
	var asJSON bool
	c := &cobra.Command{Use: "usage", Short: "show measured offer calls and units", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if by != primary && by != "tool" {
			return fmt.Errorf("--by must be %s or tool", primary)
		}
		state, err := door.StateDir()
		if err != nil {
			return err
		}
		path := filepath.Join(state, "audit.jsonl")
		dir := filepath.Join(filepath.Dir(state), "offers")
		if remote {
			path, err = broker.RemoteUsagePath()
			if err != nil {
				return err
			}
			dir = filepath.Join(filepath.Dir(path), "remote-offers")
		}
		opts := broker.UsageRollupOptions{By: by}
		if tag != "" {
			opts.Digests, err = usageDigests(dir, tag)
			if err != nil {
				return err
			}
			if remote {
				commands, _ := fleet.New(catalogOptions...).Commands()
				for _, cmd := range commands {
					if m, ok := mountOf(cmd); ok && slices.Contains(m.Tags, tag) {
						opts.Digests[m.Digest] = true
					}
				}
			}
		}
		var input io.Reader = strings.NewReader("")
		f, err := os.Open(path)
		if err == nil {
			defer f.Close()
			input = f
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		rows, err := broker.RollupUsage(input, opts)
		if err != nil {
			return err
		}
		results := make([]map[string]any, 0, len(rows))
		for _, row := range rows {
			if asJSON {
				results = append(results, map[string]any{by: row.Principal, "calls": row.Requests, "units": row.Units})
			} else {
				units, _ := json.Marshal(row.Units)
				fmt.Fprintf(c.OutOrStdout(), "%s\t%d calls\t%s\n", row.Principal, row.Requests, units)
			}
		}
		if asJSON {
			return json.NewEncoder(c.OutOrStdout()).Encode(results)
		}
		return nil
	}}
	c.Flags().StringVar(&by, "by", primary, "group by "+primary+" or tool")
	c.Flags().StringVar(&tag, "tag", "", "filter by definition tag")
	c.Flags().BoolVar(&asJSON, "json", false, "machine-readable usage")
	return c
}
