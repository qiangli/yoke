package fleet

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// AppliedDefaults is the result of matching a command line against the glob
// defaults of a resolved ReaderPrefs, PSDefaultParameterValues-style.
type AppliedDefaults struct {
	Options map[string]any    // option name (underscored) -> value
	Sources map[string]string // option name -> layer that supplied the winning value
	Pattern map[string]string // option name -> glob that supplied the winning value
}

// Empty reports whether no default matched.
func (a AppliedDefaults) Empty() bool { return len(a.Options) == 0 }

func globSpecificity(p string) int {
	n := 0
	for _, r := range p {
		if r != '*' && r != '?' {
			n++
		}
	}
	return n
}

// MatchCommandDefaults returns the defaults whose glob matches command (the
// command name plus subcommand words, e.g. "graph build"). When several globs
// set the same option the most specific glob (most literal characters, then
// lexical order) wins, so "*" is a floor and "rm" overrides it.
func MatchCommandDefaults(prefs ReaderPrefs, sources ReaderPrefsSources, command string) AppliedDefaults {
	command = strings.Join(strings.Fields(command), " ")
	var pats []string
	for pat := range prefs.Defaults {
		if ok, err := path.Match(pat, command); err == nil && ok {
			pats = append(pats, pat)
		}
	}
	sort.Slice(pats, func(i, j int) bool {
		si, sj := globSpecificity(pats[i]), globSpecificity(pats[j])
		if si != sj {
			return si < sj
		}
		return pats[i] < pats[j]
	})
	out := AppliedDefaults{Options: map[string]any{}, Sources: map[string]string{}, Pattern: map[string]string{}}
	for _, pat := range pats {
		for k, v := range prefs.Defaults[pat] {
			k = strings.ReplaceAll(k, "-", "_")
			out.Options[k] = v
			src := sources.SourceForDefault(pat, k)
			if src == "" {
				src = sources.SourceForDefault(pat, strings.ReplaceAll(k, "_", "-"))
			}
			out.Sources[k] = src
			out.Pattern[k] = pat
		}
	}
	return out
}

// Flags renders the applied defaults as long-option arguments in sorted order.
// Booleans become --name / --name=false; everything else --name=value.
func (a AppliedDefaults) Flags() []string {
	keys := make([]string, 0, len(a.Options))
	for k := range a.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []string
	for _, k := range keys {
		name := "--" + strings.ReplaceAll(k, "_", "-")
		switch v := a.Options[k].(type) {
		case bool:
			if v {
				args = append(args, name)
			} else {
				args = append(args, name+"=false")
			}
		default:
			args = append(args, fmt.Sprintf("%s=%v", name, v))
		}
	}
	return args
}

// Explain lists each applied default with its source layer and glob, never
// the values of anything but the declared option itself.
func (a AppliedDefaults) Explain() []string {
	keys := make([]string, 0, len(a.Options))
	for k := range a.Options {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, fmt.Sprintf("default %s=%v source=%s pattern=%q", k, a.Options[k], a.Sources[k], a.Pattern[k]))
	}
	return lines
}

// IsZero lets encoding/json's omitzero drop an undeclared prefs block, which
// omitempty cannot do for a struct with a custom MarshalJSON.
func (p ReaderPrefs) IsZero() bool { return p.IsEmpty() }
