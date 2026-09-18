package role

import (
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func TestOwnerFlagUsesOneSpellingAndTheDomainTitle(t *testing.T) {
	for _, title := range []Title{Facilitator, ProjectManager, Assignee} {
		f := pflag.NewFlagSet("test", pflag.ContinueOnError)
		var owner string
		AttachOwner(f, &owner, title, "does the work")
		if err := f.Parse([]string{"--owner", "agent-one"}); err != nil {
			t.Fatal(err)
		}
		if owner != "agent-one" {
			t.Fatalf("%s owner = %q", title, owner)
		}
		help := f.FlagUsages()
		if !strings.Contains(help, "--owner") || !strings.Contains(help, string(title)) {
			t.Fatalf("%s help = %q", title, help)
		}
	}
}
