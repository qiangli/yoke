package craft

import (
	"testing"

	"github.com/spf13/cobra"
)

// THE RULE, generalised from a real collision: a command whose own positional
// argument is an ARBITRARY USER TOKEN must not have subcommands, because every
// subcommand name permanently removes that word from what the argument can
// express — and nothing fails until somebody uses exactly that word.
//
// `bashy define study` was the case that found it: mounting a `study`
// subcommand meant "define the word study" became unaskable, silently.
//
// craft itself is safe (its own arguments are a closed set of verbs, and
// `study` takes a PATH, which cannot collide with a sibling verb name). But
// find and compose take free text and are one careless addition away from the
// same trap, so they are pinned here.
func TestFreeTextVerbs_HaveNoSubcommands(t *testing.T) {
	root := NewCraftCmd()
	for _, name := range []string{"find", "compose"} {
		var target = findSub(root, name)
		if target == nil {
			t.Fatalf("%q is no longer a craft subcommand; update this ratchet", name)
		}
		if subs := target.Commands(); len(subs) != 0 {
			t.Errorf("%q takes free text and now has subcommands — each one steals that word "+
				"from every query", name)
		}
	}
}

func findSub(root *cobra.Command, name string) *cobra.Command {
	for _, c := range root.Commands() {
		if c.Name() == name {
			return c
		}
	}
	return nil
}
