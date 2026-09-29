package fleet

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// Migration is intentionally two steps. Diffing a full copy against today's
// lower entry cannot recover which equal values were deliberately pinned, or
// which absent values were deliberately cleared. The candidate is a review aid;
// only an operator supplied overlay is applied, after an effective-value check.
func newMigrateOverride(noun string, opts []Option) *cobra.Command {
	var reviewed string
	c := &cobra.Command{
		Use:   "migrate <name>",
		Short: "Review and migrate a full-copy local override to a sparse overlay",
		Long:  "Print a candidate overlay. Review every field, then pass the reviewed YAML with --reviewed FILE. The command checks that the effective record is unchanged and saves the old file as .yaml.bak before replacing it.",
		Args:  cobra.ExactArgs(1), SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			name := args[0]
			fileName := name
			if foundFile, _, ok := cat.lowerEntry(noun, name); ok {
				fileName = foundFile
			}
			path, err := entryPath(cat.nounDir(noun), fileName)
			if err != nil {
				return err
			}
			old, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if isOverlay(old) {
				return fmt.Errorf("fleet: %q is already an overlay", name)
			}
			_, lower, ok := cat.lowerEntry(noun, name)
			if !ok {
				return fmt.Errorf("fleet: %q has no lower entry to overlay", name)
			}
			if reviewed == "" {
				candidate, err := migrationCandidate(noun, name, lower, old)
				if err != nil {
					return err
				}
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n# Review equal-value pins and intentional clears before applying.\n", candidate)
				return err
			}
			proposal, err := os.ReadFile(reviewed)
			if err != nil {
				return err
			}
			if !isOverlay(proposal) {
				return fmt.Errorf("fleet: reviewed file must declare overlay: true")
			}
			merged, err := mergeOverlay(lower, proposal, noun)
			if err != nil {
				return err
			}
			oldValue, err := parseMigrationValue(noun, name, old)
			if err != nil {
				return err
			}
			newValue, err := parseMigrationValue(noun, name, merged)
			if err != nil {
				return err
			}
			if !bytes.Equal(oldValue, newValue) {
				return fmt.Errorf("fleet: reviewed overlay changes the effective %s %q; revise it before migration", noun, name)
			}
			backup := path + ".bak"
			f, err := os.OpenFile(backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return fmt.Errorf("fleet: backup %s: %w", backup, err)
			}
			if _, err = f.Write(old); err != nil {
				f.Close()
				return err
			}
			if err = f.Close(); err != nil {
				return err
			}
			if err = writeEntry(cat.nounDir(noun), fileName, proposal); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "migrated %s; backup: %s\n", name, backup)
			return err
		},
	}
	c.Flags().StringVar(&reviewed, "reviewed", "", "apply this reviewed sparse YAML after preserving a backup")
	return c
}

func migrationCandidate(noun, name string, lower, old []byte) (string, error) {
	base, err := decodeMap(lower)
	if err != nil {
		return "", err
	}
	local, err := decodeMap(old)
	if err != nil {
		return "", err
	}
	var patch yamlMap
	if noun == dirAgents {
		bs, bok := base["agents"].([]any)
		ls, lok := local["agents"].([]any)
		if !bok {
			bs = []any{base}
		}
		if !lok {
			ls = []any{local}
		}
		var patches []any
		for _, item := range ls {
			l, ok := item.(yamlMap)
			if !ok {
				return "", fmt.Errorf("fleet: invalid local agent")
			}
			identity, _ := l["name"].(string)
			if identity == "" {
				identity = name
			}
			var b yamlMap
			for _, row := range bs {
				r, ok := row.(yamlMap)
				if ok && (r["name"] == identity || r["name"] == nil && identity == name) {
					b = r
					break
				}
			}
			if b == nil {
				return "", fmt.Errorf("fleet: lower agent %q is missing", identity)
			}
			p := explicitDiff(b, l)
			p["name"] = identity
			patches = append(patches, p)
		}
		patch = yamlMap{"overlay": true, "agents": patches}
		for k, v := range explicitDiff(base, local) {
			if k != "agents" {
				patch[k] = v
			}
		}
	} else {
		patch = explicitDiff(base, local)
		patch["overlay"] = true
		patch["name"] = name
	}
	data, err := yaml.Marshal(patch)
	return strings.TrimSpace(string(data)), err
}

// Only fields actually present in the old file are proposed. Missing fields
// may be intentional clears and must be reviewed rather than guessed.
func explicitDiff(base, local yamlMap) yamlMap {
	out := yamlMap{}
	for k, v := range local {
		if k == "overlay" {
			continue
		}
		if b, ok := base[k].(yamlMap); ok {
			if l, ok := v.(yamlMap); ok {
				if d := explicitDiff(b, l); len(d) > 0 {
					out[k] = d
				}
				continue
			}
		}
		if !reflect.DeepEqual(base[k], v) {
			out[k] = v
		}
	}
	return out
}

func parseMigrationValue(noun, name string, data []byte) ([]byte, error) {
	switch noun {
	case dirTools:
		v, err := ParseTool(name, data, nil)
		if err != nil {
			return nil, err
		}
		return Marshal(v)
	case dirModels:
		v, err := ParseModel(name, data, nil)
		if err != nil {
			return nil, err
		}
		return Marshal(v)
	case dirAgents:
		v, err := ParseAgentFile(name, data, nil)
		if err != nil {
			return nil, err
		}
		return Marshal(v)
	}
	return nil, fmt.Errorf("unknown fleet noun %q", noun)
}
