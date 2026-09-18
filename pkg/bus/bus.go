// Package bus is the agent notification bus: the PUSH half of how agents on a
// host coordinate, where `bashy kb` is the durable PULL half.
//
// The pairing is the whole idea. kb answers "what is true about this host" and
// is read when an agent goes looking; the bus answers "something just changed"
// and reaches an agent that was not looking. A fact that only lives in kb is
// found by whoever thinks to search for it; a change that only lives on the bus
// is missed by whoever was busy. Neither replaces the other.
//
// The motivating case is parallel-work de-conflict: agent A publishes that it is
// editing a file, agent B — watching — course-corrects before the merge conflict
// rather than after it.
//
//	bashy bus publish --topic build "pkg/ask landed — rebase first"
//	bashy bus watch --topic build --drain
//
// # Storage
//
// The transport is the host room's timeline (pkg/room): an append-only JSONL log
// under ~/.bashy/room. Publishing appends one event; watching reads them back.
// There is no daemon and no broker — a process that is not running cannot drop a
// message, and the log is the same one `bashy chat timeline` renders.
//
// # Naming
//
// This replaces the never-reachable `bashy notify`. The design sketch called the
// subscriber `bashy watch`, which cannot work: `watch` is a registered coreutils
// tool — the classic watch(1) that runs a command periodically — and shadowing a
// POSIX-familiar command to gain a bus verb is a bad trade. A `bus` parent gives
// both halves a home and makes the pairing legible at the command line.
package bus

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
)

// SchemaVersion tags every JSON envelope this package emits.
const SchemaVersion = "bashy-bus-v1"

// NewBusCmd returns the `bus` command tree — the host-agnostic entry point a
// front end mounts (e.g. `bashy bus`), mirroring secrets.NewSecretsCmd.
func NewBusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bus",
		Short: "agent notification bus: publish a change, watch for one",
		Long: `bus is how agents on this host tell each other something changed.

It is the push half of a pair: 'bashy kb' holds what is TRUE about the host and
is read when an agent goes looking; the bus carries what just CHANGED and
reaches an agent that was not looking.

  bashy bus publish --topic build "pkg/ask landed — rebase first"
  bashy bus watch --topic build --drain

The transport is the host room's append-only timeline, the same log
'bashy chat timeline' renders. There is no daemon and no broker, so nothing can
be down and silently swallow a message.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(
		newPublishCmd(), newWatchCmd(),
		// The sidecar half: an agent mid-turn cannot watch a channel, so these
		// hold the subscription for it and hand it a pre-resolved buffer.
		newSubscribeCmd(), newUnsubscribeCmd(), newSubscriptionsCmd(),
		newSidecarCmd(), newPendingCmd(),
	)
	return cmd
}

// ErrReported marks a failure the command has ALREADY written to stderr, in
// whichever form the caller asked for. A front end checks Reported and prints
// nothing more.
//
// Without it the two output modes cannot both be correct: --json writes a
// `"status":"error"` envelope to stderr, and a front end that also prints the
// returned error appends a plain line to what is supposed to be a parseable
// stream. One reporting path, one place that decides the wording.
var ErrReported = errors.New("bus: already reported")

// ExitCode maps a RunE error to a process exit status, mirroring the
// skills.ExitCode / dag.ExitCodeOf convention the other front-door verbs use.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	return 1
}

// Reported reports whether the command has already written this failure to
// stderr, so a front end knows not to print it again.
func Reported(err error) bool { return errors.Is(err, ErrReported) }

// reported wraps an error as already-written-to-stderr.
func reported(format string, a ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, a...), ErrReported)
}
