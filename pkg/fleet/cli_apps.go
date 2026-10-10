package fleet

import "github.com/spf13/cobra"

// NewAppCmds builds the registered-app CRUD words the Apps console mounts
// under its own `app` verb: show · schema · add · set · rm · edit. `list` is
// the console's (it merges every panel source and probes liveness).
func NewAppCmds(opts ...Option) []*cobra.Command {
	return []*cobra.Command{
		newShow(KindApp, opts),
		newSchema(KindApp),
		NewRetireCmd(KindApp, opts...), NewUnretireCmd(KindApp, opts...),
		newAdd(KindApp, opts),
		newSet(KindApp, opts),
		newRm(KindApp, opts, (*Catalog).RemoveApp),
		newEdit(KindApp, opts, (*Catalog).MaterializeApp),
	}
}

// AppsHint is printed after a write: the console mounts panels when it
// starts, so a running one picks the change up on restart.
const AppsHint = "takes effect when the console (re)starts: bashy app service stop && bashy app service start"
