package fleet

import "github.com/spf13/cobra"

// NewAppCmds builds the registered-app CRUD words the Apps console mounts
// under its own `app` verb: show · schema · add · new · set · rm · edit. `list` is
// the console's (it merges every panel source and probes liveness).
func NewAppCmds(opts ...Option) []*cobra.Command {
	return []*cobra.Command{
		newShow(KindApp, opts),
		newSchema(KindApp),
		NewRetireCmd(KindApp, opts...), NewUnretireCmd(KindApp, opts...),
		newAdd(KindApp, opts),
		newAppNew(opts),
		newSet(KindApp, opts),
		newRm(KindApp, opts, (*Catalog).RemoveApp),
		newEdit(KindApp, opts, (*Catalog).MaterializeApp),
	}
}

// AppsHint is printed after a write: production consoles discover registered
// records per request, so registration and linked-site edits are live.
const AppsHint = "takes effect on the console's next request"
