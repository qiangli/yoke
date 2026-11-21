package bus

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/admission"
	"github.com/qiangli/yoke/pkg/room"
)

// DefaultTurnAdmissionBytes is the combined automatic Bus and host-inbox
// ceiling. Managed PTY sessions can accept at most 1024 bytes in one injected
// inbox block; exceeding that limit would withhold every source receipt and
// replay the same batch on the next turn.
const DefaultTurnAdmissionBytes = 1024

// PreparedPreamble is a rendered inbox snapshot whose cursor acknowledgements
// are deliberately deferred until the embedding host has injected Text into a
// live agent successfully.
type PreparedPreamble struct {
	Text   string
	ack    []func() error
	report admission.Report
	err    error
}

// NewPreparedPreamble creates a host-owned preamble without exposing the
// acknowledgement list as mutable API.
func NewPreparedPreamble(text string, ack func() error) PreparedPreamble {
	p := PreparedPreamble{Text: strings.TrimSpace(text)}
	if ack != nil {
		p.ack = append(p.ack, ack)
	}
	return p
}

// Commit acknowledges the exact source snapshots represented by Text. Call it
// only after successful delivery. Repeated calls are safe because every source
// cursor is monotonic.
func (p PreparedPreamble) Commit() error {
	if p.err != nil {
		return p.err
	}
	for _, ack := range p.ack {
		if err := ack(); err != nil {
			return err
		}
	}
	return nil
}

// Err reports a preparation/rendering failure before a host attempts delivery.
func (p PreparedPreamble) Err() error { return p.err }

// AdmissionReport returns body-free byte/item accounting and digests suitable
// for telemetry.
func (p PreparedPreamble) AdmissionReport() admission.Report { return p.report }

// PrepareTurnInbox is the embedding host's read-through view of communication
// stores that pkg/bus cannot import (for example Meet, which already imports
// bus). It owns no storage and must acknowledge only records it successfully
// rendered. Bashy wires it to its unified inbox; a bare coreutils embedding
// leaves it nil and retains the bus-only behaviour.
var PrepareTurnInbox func(agent string) PreparedPreamble

// PrepareTurnItems is the lossless Bashy unified-inbox adapter. The host maps
// its existing stores to neutral records with stable retrieval references and
// exact per-record acknowledgements. pkg/bus deliberately does not import those
// stores. PrepareTurnInbox remains as a compatibility bridge for older hosts.
var PrepareTurnItems func(agent string) ([]admission.Item, error)

// TurnPreamble returns the pending-notification block for the live session
// reachable at ctlSock, and CLEARS what it returns.
//
// This is the turn-boundary inject point, called by whoever is about to hand an
// agent a turn. It closes the last gap in the bus: `bus pending` is a channel an
// agent must choose to read, and the whole premise of the sidecar is that an
// agent cannot reliably choose. So the harness reads it on the agent's behalf, at
// the one moment the agent is guaranteed to be listening — the instant it is
// being given something to do.
//
// Sessions are matched by CONTROL SOCKET rather than by name. A subscription
// already names the instance it belongs to (so the sidecar knows where to send an
// interrupt), and resolving that instance to its live room card yields the socket
// — so the same link serves both directions and no new identity field is needed.
// Matching on a name would also be wrong: names are reused across runs, and a
// stale subscription would hand one session another's notifications.
//
// Best-effort throughout, and deliberately so: an unreadable bus must never block
// a steer. The same discipline kb uses — a missing store costs nothing and stops
// nothing.
func TurnPreamble(ctlSock string) string {
	p := PrepareTurnPreamble(ctlSock)
	_ = p.Commit()
	return p.Text
}

// PrepareTurnPreamble snapshots pending input for a verified live control
// socket but does not advance any cursor. The caller commits after injection.
func PrepareTurnPreamble(ctlSock string) PreparedPreamble {
	if strings.TrimSpace(ctlSock) == "" {
		return PreparedPreamble{}
	}
	sub, ok := subscriberForCtlSock(ctlSock)
	if !ok {
		return PreparedPreamble{}
	}
	// A sidecar is an immediacy optimization, not a correctness prerequisite.
	// Reconcile the timeline and pending view now so a Bashy-owned session still
	// receives Bus input when no sidecar process is running.
	snapshot, err := SnapshotInbox(sub)
	if err != nil {
		return PreparedPreamble{}
	}
	return prepareAdmission(sub, snapshot)
}

// subscriberForCtlSock finds the subscription whose instance is the live session
// on this control socket.
func subscriberForCtlSock(ctlSock string) (string, bool) {
	subs, err := Subscriptions()
	if err != nil {
		return "", false
	}
	for _, s := range subs {
		if s.Instance == "" {
			continue
		}
		card, found, ferr := room.Find(s.Instance)
		if ferr != nil || !found {
			continue
		}
		if card.CtlSock != "" && card.CtlSock == ctlSock {
			return s.Subscriber, true
		}
	}
	return "", false
}

// Prepend puts any pending notifications in front of the text about to be sent
// to a session.
//
// The block goes FIRST, ahead of the caller's own message, because a
// notification is context for the instruction that follows — "Foo was renamed"
// changes how the agent should read "now fix Foo". Appending it after would have
// the agent commit to an approach and only then learn the ground moved.
func Prepend(ctlSock, text string) string {
	p := PreparePrepend(ctlSock, text)
	_ = p.Commit()
	return p.Text
}

// PreparePrepend is Prepend with deferred cursor acknowledgement.
func PreparePrepend(ctlSock, text string) PreparedPreamble {
	p := PrepareTurnPreamble(ctlSock)
	if p.Text == "" {
		p.Text = text
		return p
	}
	p.Text = p.Text + "\n" + text
	return p
}

// LaunchPreamble is the compatibility immediate-ack form. Bashy chat uses the
// prepared form below so a refused launch cannot consume its mail.
func LaunchPreamble(agent string) string {
	p := PrepareLaunchPreamble(agent)
	_ = p.Commit()
	return p.Text
}

// PrepareLaunchPreamble snapshots everything addressed to agent without
// advancing cursors. The caller commits after the launch accepts the prompt.
func PrepareLaunchPreamble(agent string) PreparedPreamble {
	agent = strings.TrimSpace(agent)
	if agent == "" {
		return PreparedPreamble{}
	}
	snapshot, err := SnapshotInbox(agent)
	if err != nil {
		return PreparedPreamble{}
	}
	return prepareAdmission(agent, snapshot)
}

// inboxMaxItems bounds one turn preamble to a small, fixed number of inline
// messages plus one counter line, no matter how large the unread backlog is.
// A fresh binding with no cursor starts at the beginning of history, and an
// unbounded render hands it that whole history every turn (with only
// represented records acknowledged, the rest is re-sent forever). The default
// shows one message — addressed to this agent first, then highest priority,
// then newest — and counts the rest. BASHY_CHAT_INBOX_MAX=N raises the inline
// count; BASHY_CHAT_INBOX=off (honored by the chat front door) is unchanged.
func inboxMaxItems() int {
	const defaultInboxMaxItems = 1
	raw := strings.TrimSpace(os.Getenv("BASHY_CHAT_INBOX_MAX"))
	if raw == "" {
		return defaultInboxMaxItems
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return defaultInboxMaxItems
	}
	return n
}

// orderInboxFor sorts a turn's items the way the cap spends them: messages
// addressed to this agent (or the role it reads as) first, then highest
// priority, then newest.
func orderInboxFor(agent string, items []admission.Item) {
	agent = strings.TrimSpace(agent)
	sort.SliceStable(items, func(i, j int) bool {
		a, b := items[i], items[j]
		ad, bd := isAddressedTo(agent, a), isAddressedTo(agent, b)
		if ad != bd {
			return ad
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.Sequence > b.Sequence
	})
}

func isAddressedTo(agent string, item admission.Item) bool {
	if agent == "" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(item.To), agent)
}

// splitInbox divides a turn's ordered items into the inline head and the
// counted tail. max <= 0 means no cap.
func splitInbox(items []admission.Item, max int) (show, withheld []admission.Item) {
	if max > 0 && len(items) > max {
		return items[:max], items[max:]
	}
	return items, nil
}

// inboxCounterLine is the one indicator line for a capped tail: how many were
// shown of how many unread, how many of the unread ones are addressed to this
// agent, and the one command that reads them. The withheld records stay
// durable in their stores; this line is what keeps the cap from being a
// silent drop.
func inboxCounterLine(agent string, admitted, total, addressed int) string {
	return fmt.Sprintf("%d of %d unread (%d addressed to you) — read more: bashy inbox --as %s",
		admitted, total, addressed, strconv.Quote(agent))
}

func inboxBytes(items []admission.Item) int {
	n := 0
	for _, item := range items {
		n += len(item.Body)
	}
	return n
}

func countAddressedTo(agent string, items []admission.Item) int {
	n := 0
	for _, item := range items {
		if isAddressedTo(agent, item) {
			n++
		}
	}
	return n
}

func prepareAdmission(agent string, snapshot InboxSnapshot) PreparedPreamble {
	items := make([]admission.Item, 0, len(snapshot.Items)+1)
	for _, pending := range snapshot.Items {
		p := pending
		items = append(items, admission.Item{
			Source: "bus", ID: strconv.FormatInt(p.Seq, 10), Sequence: p.Seq,
			From: p.Principal, To: p.To, Topic: p.Topic,
			Priority: classifyPending(agent, p), Body: p.Body,
			ArtifactRef: fmt.Sprintf("bashy inbox --as %s --id bus:%d --peek", strconv.Quote(agent), p.Seq),
			OverflowRef: "bashy inbox --as " + strconv.Quote(agent) + " --peek",
			Acknowledge: func() error { return snapshot.CommitItem(p.Seq) },
		})
	}
	var host []admission.Item
	if PrepareTurnItems != nil {
		var err error
		host, err = PrepareTurnItems(agent)
		if err != nil {
			return PreparedPreamble{err: err}
		}
		items = append(items, host...)
	}
	orderInboxFor(agent, items)
	claimed := make([]bool, len(items))
	var rendered admission.Prepared
	var show, withheld []admission.Item
	for {
		show, withheld = splitInbox(items, inboxMaxItems())
		// A counted tail is only honest when every withheld record names where to
		// read it. This is the same floor admission.Render holds for its own
		// unrepresented set: without a retrieval reference the counter line would
		// be a silent drop, so the preamble fails instead of consuming it.
		for _, item := range withheld {
			if strings.TrimSpace(item.OverflowRef) == "" {
				return PreparedPreamble{err: fmt.Errorf("admission: unrepresented item %s:%s has no source retrieval reference", item.Source, item.ID)}
			}
		}
		var err error
		rendered, err = admission.Render(show, admission.Options{BudgetBytes: DefaultTurnAdmissionBytes})
		if err != nil {
			return PreparedPreamble{err: err}
		}
		rejected := map[int]bool{}
		for _, idx := range rendered.AdmittedIndices() {
			if idx < 0 || idx >= len(show) || claimed[idx] {
				continue
			}
			if show[idx].Claim != nil {
				if show[idx].Claim() {
					claimed[idx] = true
				} else {
					rejected[idx] = true
				}
			}
		}
		if len(rejected) == 0 {
			break
		}
		kept := make([]admission.Item, 0, len(items)-len(rejected))
		nextClaimed := make([]bool, 0, len(items)-len(rejected))
		for i, item := range items {
			if i < len(show) && rejected[i] {
				continue
			}
			kept = append(kept, item)
			nextClaimed = append(nextClaimed, claimed[i])
		}
		items = kept
		claimed = nextClaimed
	}
	if len(withheld) == 0 {
		prepared := PreparedPreamble{Text: rendered.Text, ack: []func() error{rendered.Commit}, report: rendered.Report}
		return mergeLegacyHost(agent, prepared)
	}
	// The cap withheld records this turn only counts. The counter line tells
	// the agent exactly that, so committing advances every watermark past the
	// whole accounted snapshot — shown and counted alike. DEMOTE, NEVER DROP:
	// no store deletes on acknowledgement, and every record stays retrievable
	// by its artifact reference.
	text := joinPreambles(rendered.Text, inboxCounterLine(agent,
		rendered.Report.AdmittedItems, len(items), countAddressedTo(agent, items)))
	report := rendered.Report
	report.InputItems = len(items)
	report.InputBytes = inboxBytes(items)
	report.OmittedItems += len(withheld)
	report.OmittedBytes += inboxBytes(withheld)
	report.UnrepresentedItems += len(withheld)
	report.UnrepresentedBytes += inboxBytes(withheld)
	// When the host adapter tracks turn delivery separately from explicit
	// acknowledgement (MarkDelivered != nil), commit only the items actually
	// rendered inline so withheld items stay unread and retrievable via
	// `bashy inbox`. Legacy adapters without MarkDelivered keep advancing all
	// host items alongside the bus snapshot.
	acks := []func() error{func() error { return snapshot.Commit() }}
	hasSeparateDelivery := false
	for _, h := range host {
		if h.MarkDelivered != nil {
			hasSeparateDelivery = true
			break
		}
	}
	if hasSeparateDelivery {
		acks = append(acks, rendered.Commit)
		for _, h := range host {
			if h.MarkDelivered == nil && h.Acknowledge != nil {
				acks = append(acks, h.Acknowledge)
			}
		}
	} else {
		for _, h := range host {
			if h.Acknowledge != nil {
				acks = append(acks, h.Acknowledge)
			}
		}
	}
	prepared := PreparedPreamble{Text: text, ack: acks, report: report}
	return mergeLegacyHost(agent, prepared)
}

// mergeLegacyHost preserves the exact delivery contract for hosts that still
// wire the whole-snapshot PrepareTurnInbox callback instead of the
// record-scoped PrepareTurnItems adapter.
func mergeLegacyHost(agent string, prepared PreparedPreamble) PreparedPreamble {
	// The legacy callback represents a whole host-owned snapshot with one
	// acknowledgement. It cannot safely participate in record-scoped admission:
	// reducing it to one header and then committing would consume bodies that the
	// delivered retrieval command may no longer show as unread. Preserve the old
	// exact delivery contract until the host wires PrepareTurnItems. When the item
	// adapter is present it is authoritative, so do not also invoke the legacy
	// callback and duplicate the same messages.
	if PrepareTurnItems == nil && PrepareTurnInbox != nil {
		legacy := PrepareTurnInbox(agent)
		if legacy.err != nil {
			return PreparedPreamble{err: legacy.err}
		}
		prepared = mergePrepared(prepared, legacy)
	}
	return prepared
}

func classifyPending(agent string, p Pending) admission.Priority {
	directed := strings.TrimSpace(p.To) != "" && strings.EqualFold(strings.TrimSpace(p.To), strings.TrimSpace(agent))
	header := strings.ToUpper(strings.Join([]string{p.Topic, firstLine(p.Body)}, " "))
	if directed && headerHasAnyWord(header, "BLOCKED", "CONFLICT", "STOP", "SECURITY", "OWNERSHIP") {
		return admission.PriorityUrgent
	}
	if directed && (headerHasAnyWord(header, "REQUEST", "REPLY", "ACK") || strings.Contains(p.Body, "?")) {
		return admission.PriorityResponse
	}
	if headerHasAnyWord(header, "DECISION", "BASELINE", "FAILED", "FAILURE") {
		return admission.PriorityDecision
	}
	if directed || p.Topic != "" {
		return admission.PriorityDirected
	}
	return admission.PriorityInformational
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func headerHasAnyWord(s string, wants ...string) bool {
	for _, word := range strings.FieldsFunc(s, func(r rune) bool { return !(r >= 'A' && r <= 'Z') }) {
		for _, want := range wants {
			if word == want {
				return true
			}
		}
	}
	return false
}

func mergePrepared(a, b PreparedPreamble) PreparedPreamble {
	a.Text = joinPreambles(a.Text, b.Text)
	a.ack = append(a.ack, b.ack...)
	return a
}

// PrepareForAgent puts a cold agent's unread mail ahead of its opening prompt
// and defers acknowledgement until the prompt is accepted.
func PrepareForAgent(agent, text string) PreparedPreamble {
	p := PrepareLaunchPreamble(agent)
	if p.Text == "" {
		p.Text = text
		return p
	}
	p.Text = p.Text + "\n" + text
	return p
}

func joinPreambles(a, b string) string {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	switch {
	case a == "":
		return b
	case b == "":
		return a
	default:
		return a + "\n\n" + b
	}
}

// PrependForAgent puts an agent's unread mail in front of the text it is about
// to be given. Returns text unchanged when there is nothing.
func PrependForAgent(agent, text string) string {
	p := PrepareForAgent(agent, text)
	_ = p.Commit()
	return p.Text
}
