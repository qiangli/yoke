package fleet

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// CreatePerson adds a person to the local store. Unlike SavePerson (an
// unconditional write, used by set), it refuses a handle or alias that
// already names someone, the same name rule every other registry noun
// follows; force takes the name anyway.
func (c *Catalog) CreatePerson(p Person, force bool) error {
	if !force {
		if existing, ok := c.Person(p.Handle); ok {
			return fmt.Errorf("fleet: person %q already exists (%s ring); change it with `bashy person set`, or pass --force to replace it",
				p.Handle, existing.Ring)
		}
		if err := c.claimName(KindPerson, p.Handle, p.Aliases, false); err != nil {
			return err
		}
	}
	return c.SavePerson(p)
}

// MaterializePerson returns the local-store file for a person, copying an
// entry from another ring into the local store first (what edit opens).
func (c *Catalog) MaterializePerson(name string) (string, error) {
	p, ok := c.Person(name)
	if !ok {
		return "", fmt.Errorf("fleet: no person %q", name)
	}
	if p.Ring != ringLocal() {
		if err := c.SavePerson(p); err != nil {
			return "", err
		}
	}
	return entryPath(c.nounDir(dirPeople), p.Handle)
}

// The generic verb builders, exported for registry nouns whose CLI lives
// outside this package (person in pkg/principal), so every noun shares one
// implementation of edit, schema and show output.

// NewEditCmd is the shared `edit <name>` verb: materialize, then $EDITOR.
func NewEditCmd(noun string, opts []Option, materialize func(*Catalog, string) (string, error)) *cobra.Command {
	return newEdit(noun, opts, materialize)
}

// NewSchemaCmd is the shared `schema` verb for a noun known to nounType.
func NewSchemaCmd(noun string) *cobra.Command { return newSchema(noun) }

// Emit writes an entry as YAML, or JSON when asJSON, exactly as `show` does.
func Emit(w io.Writer, v any, asJSON bool) error { return emit(w, v, asJSON) }
