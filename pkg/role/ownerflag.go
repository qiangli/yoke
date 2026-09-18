// The ONE accountability flag every durable object shares, kept here so meet,
// sprint and todo cannot drift apart on how an owner is named.
package role

import (
	"fmt"

	"github.com/spf13/pflag"
)

// Title is what an owner is CALLED in the domain that owns it. The flag is one
// word everywhere so an agent learns it once; the title is the domain's own
// word so the help still reads like the thing it describes.
type Title string

const (
	// Facilitator runs a meeting's floor and answers for the room.
	Facilitator Title = "facilitator"
	// ProjectManager is accountable for a sprint's delivery start to end.
	ProjectManager Title = "project manager"
	// Assignee is who is working an item.
	Assignee Title = "assignee"
)

// AttachOwner registers the one ownership flag. The spelling is identical in
// every domain; the title explains what that owner is called there.
func AttachOwner(f *pflag.FlagSet, target *string, title Title, what string) {
	f.StringVar(target, "owner", "", fmt.Sprintf("%s — %s", title, what))
}
