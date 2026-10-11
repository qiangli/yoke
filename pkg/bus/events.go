package bus

import (
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/room"
)

// EventsSchemaVersion tags every line `events --json` and /api/events emit.
const EventsSchemaVersion = "bashy-events-v1"

// KindFilter parses a comma/repeat separated --kind value into a matcher over
// Event.Type. Empty means every type.
func KindFilter(kinds []string) func(room.Event) bool {
	set := map[string]bool{}
	for _, k := range kinds {
		for _, p := range strings.Split(k, ",") {
			if p = strings.TrimSpace(p); p != "" {
				set[p] = true
			}
		}
	}
	if len(set) == 0 {
		return nil
	}
	return func(e room.Event) bool { return set[e.Type] }
}

// EventsLine is the wire form of one event: the timeline Event under the
// events schema tag. A resync uses ResyncLine.
type EventsLine struct {
	SchemaVersion string `json:"schema_version"`
	room.Event
}

// ResyncLine is the wire form of a rebase notice. Type is always "resync" and
// Seq is the position the stream continues after.
type ResyncLine struct {
	SchemaVersion string `json:"schema_version"`
	Type          string `json:"type"`
	room.Resync
}

// EncodeStreamItem renders one stream delivery as its JSON wire line.
func EncodeStreamItem(it room.StreamItem) ([]byte, error) {
	if it.Resync != nil {
		return json.Marshal(ResyncLine{SchemaVersion: EventsSchemaVersion, Type: room.EventResync, Resync: *it.Resync})
	}
	return json.Marshal(EventsLine{SchemaVersion: EventsSchemaVersion, Event: it.Event})
}

// NewEventsCmd returns the `events` verb — a thin reader over the host room
// timeline, the one event log every producer already writes. A host mounts it
// top level (`bashy events`).
func NewEventsCmd() *cobra.Command {
	var kinds []string
	var jsonOut, follow bool
	var since int64
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "events [flags]",
		Short: "read the host event log (sprint, handoff, todo, notify, ...)",
		Long: `events reads the host room timeline: the single append-only log that sprint
threads, handoffs, todo status changes, notifications and room membership all
write to. There is no second log.

  bashy events                      the live log, oldest first
  bashy events --follow             only what happens from now on
  bashy events --since 120 --follow replay after seq 120, then keep following
  bashy events --kind sprint,todo --json

Seq is global, monotonic and stable across the 7-day archive rotation, so
"--since N" resumes exactly. When it cannot -- N precedes what retention has
archived, or the log was replaced -- a resync line is printed first:

  {"type":"resync","reason":"retention","since":N,"archived_through":M,"seq":M}

and delivery continues after seq. A consumer rebases to it; nothing after it is
missing. The same stream is served as SSE at GET /api/events?from=SEQ by
'bashy meet serve'.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
			defer stop()
			explicit := cmd.Flags().Changed("since") || cmd.Flags().Changed("from")
			return room.Stream(ctx, room.StreamOptions{
				After: since, Explicit: explicit, Follow: follow, Poll: interval, Match: KindFilter(kinds),
			}, func(it room.StreamItem) error {
				return writeStreamItem(cmd, it, jsonOut)
			})
		},
	}
	f := cmd.Flags()
	f.StringSliceVar(&kinds, "kind", nil, "only these event types (comma separated: sprint,handoff,todo,notify,...)")
	f.Int64Var(&since, "since", 0, "start after this sequence number")
	f.Int64Var(&since, "from", 0, "alias for --since")
	f.BoolVar(&follow, "follow", false, "keep running and print events as they arrive")
	f.DurationVar(&interval, "interval", defaultPoll, "how often follow mode re-checks the log")
	f.BoolVar(&jsonOut, "json", false, "emit one "+EventsSchemaVersion+" JSON object per line (NDJSON)")
	return cmd
}

func writeStreamItem(cmd *cobra.Command, it room.StreamItem, jsonOut bool) error {
	w := cmd.OutOrStdout()
	if jsonOut {
		b, err := EncodeStreamItem(it)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	if r := it.Resync; r != nil {
		_, err := fmt.Fprintf(w, "-- resync (%s): was after %d, log retained from %d, continuing after %d\n",
			r.Reason, r.Since, r.ArchivedThrough, r.Seq)
		return err
	}
	e := it.Event
	line := fmt.Sprintf("%d  %s  %-8s", e.Seq, e.TS, e.Type)
	for _, p := range []string{e.Actor, e.Target, e.Topic} {
		if p != "" {
			line += "  " + p
		}
	}
	if e.Body != "" {
		line += "  " + strings.ReplaceAll(e.Body, "\n", " ")
	}
	_, err := fmt.Fprintln(w, line)
	return err
}
