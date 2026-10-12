package feed

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/lockfile"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// channelState is one channel's own delivery position. It is independent of
// the reader's read cursor: reading a post does not deliver it and delivering
// a post does not read it.
type channelState struct {
	// Seq is the highest spool seq this channel has consumed.
	Seq int64 `json:"seq"`
	// Above lists seqs delivered out of order above Seq (an immediate author's
	// post sent while older posts waited out quiet hours).
	Above []int64 `json:"above,omitempty"`
	// Last is the RFC3339 time of the last successful send.
	Last string `json:"last,omitempty"`
}

func loadChannelState(dir string) (map[string]channelState, error) {
	st := map[string]channelState{}
	data, err := os.ReadFile(channelsStatePath(dir))
	if err != nil {
		if os.IsNotExist(err) {
			return st, nil
		}
		return nil, fmt.Errorf("feed: load channel state: %w", err)
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, fmt.Errorf("feed: load channel state: %w", err)
	}
	if st == nil {
		st = map[string]channelState{}
	}
	return st, nil
}

func saveChannelState(dir string, st map[string]channelState) error {
	return writeJSON0600(dir, channelsStatePath(dir), st)
}

// pendingCandidate is a post waiting for delivery plus the spool seqs that
// stand for it (a re-fetched, edited post has several).
type pendingCandidate struct {
	Candidate
	seqs   []int64
	newest int64
}

// pendingFor maps the spool entries a channel has not yet consumed to one
// candidate per post id, newest edit winning.
func pendingFor(entries []SpoolEntry, st channelState) []pendingCandidate {
	above := make(map[int64]bool, len(st.Above))
	for _, s := range st.Above {
		above[s] = true
	}
	byID := map[string]*pendingCandidate{}
	var order []string
	for _, e := range entries {
		if e.Seq <= st.Seq || above[e.Seq] {
			continue
		}
		p, ok := byID[e.ID]
		if !ok {
			p = &pendingCandidate{}
			byID[e.ID] = p
			order = append(order, e.ID)
		}
		p.seqs = append(p.seqs, e.Seq)
		if e.Seq > p.newest {
			p.newest = e.Seq
			p.Candidate = candidateFor(e)
		}
	}
	out := make([]pendingCandidate, 0, len(order))
	for _, id := range order {
		out = append(out, *byID[id])
	}
	return out
}

func candidateFor(e SpoolEntry) Candidate {
	headline := oneLine(strings.TrimSpace(e.Headline))
	if headline == "" {
		headline = e.ID
	}
	created, err := time.Parse(time.RFC3339, e.Created)
	if err != nil {
		created, _ = time.Parse(time.RFC3339, e.Fetched)
	}
	return Candidate{
		ID: e.ID, Author: oneLine(e.Author), Headline: headline,
		Link: oneLine(postLink(e)), Created: created,
	}
}

// postLink is the post's viewer URL when the followed source is a web base
// (source + rel), else the source itself.
func postLink(e SpoolEntry) string {
	src := strings.TrimSpace(e.Source)
	if u, err := url.Parse(src); err == nil && (u.Scheme == "http" || u.Scheme == "https") && e.Rel != "" {
		return strings.TrimRight(src, "/") + "/" + strings.TrimLeft(e.Rel, "/")
	}
	return src
}

// channelPlan is what one channel would send now and what sending consumes.
type channelPlan struct {
	batch   Batch
	message string
	// consumeAll: every pending post is consumed (shown, collapsed behind the
	// "more" link, or already read). False for an immediate-only send, which
	// consumes just the immediate authors' posts.
	consumeAll bool
	consumed   []int64
	maxSeq     int64
}

// planChannel decides, without side effects, whether ch sends now.
func planChannel(ch Channel, st channelState, entries []SpoolEntry, cursor int64,
	serverRead map[string]bool, now time.Time, inboxURL string) (channelPlan, bool, error) {
	var plan channelPlan
	prefs, err := ch.Prefs()
	if err != nil {
		return plan, false, err
	}
	pend := pendingFor(entries, st)
	cands := make([]Candidate, len(pend))
	newest := map[string]int64{}
	seqs := map[string][]int64{}
	for i, p := range pend {
		cands[i] = p.Candidate
		newest[p.ID] = p.newest
		seqs[p.ID] = p.seqs
		if p.newest > plan.maxSeq {
			plan.maxSeq = p.newest
		}
	}
	read := func(id string) bool { return newest[id] <= cursor || serverRead[id] }

	var last time.Time
	if st.Last != "" {
		last, _ = time.Parse(time.RFC3339, st.Last)
	}
	batch, ok := PlanDelivery(now, last, cands, read, prefs, inboxURL)
	if !ok {
		return plan, false, nil
	}
	plan.batch, plan.message = batch, FormatBatch(batch)

	// A plan that still sends with no immediate authors is the full-batch
	// path; otherwise only the immediate authors' posts were eligible.
	probe := prefs
	probe.Immediate = nil
	_, plan.consumeAll = PlanDelivery(now, last, cands, read, probe, inboxURL)
	if !plan.consumeAll {
		imm := map[string]bool{}
		for _, a := range prefs.Immediate {
			imm[a] = true
		}
		for _, c := range cands {
			if imm[c.Author] && !read(c.ID) {
				plan.consumed = append(plan.consumed, seqs[c.ID]...)
			}
		}
	}
	return plan, true, nil
}

// advance records a successful send at now.
func (s channelState) advance(plan channelPlan, now time.Time) channelState {
	if plan.consumeAll {
		if plan.maxSeq > s.Seq {
			s.Seq = plan.maxSeq
		}
		s.Above = nil
	} else {
		s.Above = append(append([]int64(nil), s.Above...), plan.consumed...)
	}
	keep := s.Above[:0:0]
	for _, q := range s.Above {
		if q > s.Seq {
			keep = append(keep, q)
		}
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i] < keep[j] })
	s.Above = keep
	s.Last = now.UTC().Format(time.RFC3339)
	return s
}

// sendChannel runs the channel's registered tool command as
// `bashy tool cmd run CMD --yes -- ARGS --message MESSAGE` would.
func sendChannel(ctx context.Context, ch Channel, message string) error {
	tool, tc, err := toolcmd.LookupCommand(ch.Cmd)
	if err != nil {
		return err
	}
	argv, err := toolcmd.SplitArgs(ch.Args)
	if err != nil {
		return fmt.Errorf("feed: channel %q: --args: %w", ch.Name, err)
	}
	argv = append(argv, "--message", message)
	res, err := toolcmd.Run(ctx, tool, tc, strings.Join(argv, " "), toolcmd.Options{Argv: argv, Yes: true})
	if err != nil {
		return err
	}
	if res.Outcome != toolcmd.OutcomeSuccess {
		return fmt.Errorf("toolcmd: %s:%s: outcome %s", res.Tool, res.Command, res.Outcome)
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// ---- deliver ----

func newDeliverCmd(env Env) *cobra.Command {
	var only string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "deliver [--channel NAME] [--dry-run]",
		Short: "send each channel its batch of unread posts (one shot, for schedule or a service unit)",
		Long: `deliver is one-shot: for every channel that is not off it plans a batch from
the spooled posts the channel has not yet been sent and the reader has not
read, applying that channel's interval, item cap and quiet hours, and runs the
channel's registered tool command with --message set to the digest. Delivery
state advances only after the command succeeds, so a failed send is retried by
the next run. --dry-run prints each message and runs nothing.`,
		Example: `  bashy schedule add --every 30m -- bashy feed deliver
  bashy feed deliver --channel phone --dry-run`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return runDeliver(c, env, only, dryRun)
		},
	}
	cmd.Flags().StringVar(&only, "channel", "", "deliver one channel only")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print the messages and run nothing")
	return cmd
}

func runDeliver(c *cobra.Command, env Env, only string, dryRun bool) error {
	ctx := ctxOf(c)
	out, warn := c.OutOrStdout(), c.ErrOrStderr()
	dir := env.dir()

	chans, err := LoadChannels(dir)
	if err != nil {
		return err
	}
	if only != "" {
		var sel []Channel
		for _, ch := range chans {
			if ch.Name == only {
				sel = append(sel, ch)
			}
		}
		if len(sel) == 0 {
			return fmt.Errorf("feed: no channel named %q", only)
		}
		chans = sel
	}
	var active []Channel
	for _, ch := range chans {
		if !ch.Off {
			active = append(active, ch)
		}
	}
	if len(active) == 0 {
		return nil
	}

	if !dryRun {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("feed: deliver: %w", err)
		}
		lock, err := lockfile.AcquireWithin(filepath.Join(dir, "channels.lock"), 15*time.Second,
			lockfile.Holder{Name: "feed", Intent: "deliver"})
		if err != nil {
			return fmt.Errorf("feed: deliver lock: %w", err)
		}
		defer lock.Release()
	}

	entries, err := ReadSpool(dir)
	if err != nil {
		return err
	}
	cursor, err := ReadCursor(dir, subscriberName(asFlag(c)))
	if err != nil {
		return err
	}
	serverReadFn, inboxFn := env.serverRead, env.inboxURL
	if serverReadFn == nil || inboxFn == nil {
		dfltRead, dfltInbox := NewServerReadAndInbox(env)
		if serverReadFn == nil {
			serverReadFn = dfltRead
		}
		if inboxFn == nil {
			inboxFn = dfltInbox
		}
	}
	var serverRead map[string]bool
	if serverRead, err = serverReadFn(ctx); err != nil {
		fmt.Fprintf(warn, "warning: server read set unavailable (%s); using the local read cursor only\n", oneLine(err.Error()))
		serverRead = nil
	}
	inbox := inboxFn(ctx)
	st, err := loadChannelState(dir)
	if err != nil {
		return err
	}

	now := env.now().In(time.Local)
	var errs []error
	for _, ch := range active {
		plan, send, err := planChannel(ch, st[ch.Name], entries, cursor, serverRead, now, inbox)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !send {
			fmt.Fprintf(out, "channel %s: nothing to send\n", ch.Name)
			continue
		}
		n := len(plan.batch.Items) + plan.batch.More
		if dryRun {
			fmt.Fprintf(out, "channel %s: would send %s\n%s\n", ch.Name, plural(n, "item", "items"), plan.message)
			continue
		}
		if err := sendChannel(ctx, ch, plan.message); err != nil {
			errs = append(errs, fmt.Errorf("feed: channel %q: %w", ch.Name, err))
			fmt.Fprintf(warn, "channel %s: send failed: %s\n", ch.Name, oneLine(err.Error()))
			continue
		}
		st[ch.Name] = st[ch.Name].advance(plan, now)
		if err := saveChannelState(dir, st); err != nil {
			errs = append(errs, err)
			continue
		}
		fmt.Fprintf(out, "channel %s: sent %s\n", ch.Name, plural(n, "item", "items"))
	}
	return errors.Join(errs...)
}
