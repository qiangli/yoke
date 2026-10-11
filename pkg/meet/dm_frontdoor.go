package meet

// bashy meet dm AGENT --channel C --sender S [--thread T] is the external
// channel front door: a webhook relay, bot, or gateway process feeds a
// message in on argv or stdin and reads the agent's reply on stdout. It
// shares the agent's EXISTING dm-<human>-<agent> room and the chat.Invoke
// transcript path the web relay DM already uses (relay_dm.go, POST
// /api/dms/AGENT/messages) rather than keeping a second log — each distinct
// (channel, sender) just gets its own "human" identity in that same
// mechanism.
//
// A sender nobody has admitted gets a pair code and NOTHING is invoked: the
// pairing request is the SAME /approve|/deny queue (pkg/ask) a yielded
// @confirm or an ACP permission request already uses. An operator runs
// `bashy approve allow|deny <code>`; the next message from that sender
// resolves the decision, and only then does chat.Invoke ever run. The
// admission is remembered so later messages skip the pairing check — scoped
// to exactly the (agent, channel, sender) that was admitted, and revocable
// with `bashy meet dm AGENT --channel C --sender S --revoke`.
import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/spf13/cobra"
)

// dmChannelHuman is the "human" identity the shared DM room mechanism uses
// for an external message: every (channel, sender) pair gets its own durable
// dm-<human>-<agent> room, the same permanent-room mechanism the internal
// `meet dm --as` and the web relay DM already use.
func dmChannelHuman(channel, sender string) string {
	return channel + ":" + sender
}

// --- sender allowlist --------------------------------------------------------

// dmSenderGrant is one admitted (agent, channel, sender) triple. Revoked
// grants are kept, not deleted, so `meet dm --revoke` is answerable and
// re-admitting the same sender does not need a fresh pairing.
type dmSenderGrant struct {
	Agent   string    `json:"agent"`
	Channel string    `json:"channel"`
	Sender  string    `json:"sender"`
	By      string    `json:"by,omitempty"`
	Granted time.Time `json:"granted"`
	Revoked bool      `json:"revoked,omitempty"`
}

type dmSenderAllowDoc struct {
	Grants []dmSenderGrant `json:"grants,omitempty"`
}

// dmSenderAllowMu serializes this process's reads and writes of the allowlist
// file. It does not coordinate across processes; the gateway front door is
// expected to run one sender's messages through sequentially.
var dmSenderAllowMu sync.Mutex

func dmSenderAllowPath() (string, error) {
	base, err := baseDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "dm-senders.json"), nil
}

func loadDMSenderAllowDoc() (*dmSenderAllowDoc, string, error) {
	path, err := dmSenderAllowPath()
	if err != nil {
		return nil, "", err
	}
	doc := &dmSenderAllowDoc{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return doc, path, nil
		}
		return nil, "", err
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, doc); err != nil {
			return nil, "", fmt.Errorf("meet: dm sender allowlist %s is unreadable: %w", path, err)
		}
	}
	return doc, path, nil
}

func saveDMSenderAllowDoc(doc *dmSenderAllowDoc, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// dmSenderAllowed reports whether sender currently has a live, unrevoked
// grant to DM agent over channel.
func dmSenderAllowed(agent, channel, sender string) (bool, error) {
	dmSenderAllowMu.Lock()
	defer dmSenderAllowMu.Unlock()
	doc, _, err := loadDMSenderAllowDoc()
	if err != nil {
		return false, err
	}
	for _, g := range doc.Grants {
		if g.Agent == agent && g.Channel == channel && g.Sender == sender {
			return !g.Revoked, nil
		}
	}
	return false, nil
}

// dmSenderGrantAccess records that an operator's approval admitted sender on
// channel to DM agent. Granting again (after a revoke) un-revokes.
func dmSenderGrantAccess(agent, channel, sender, by string) error {
	dmSenderAllowMu.Lock()
	defer dmSenderAllowMu.Unlock()
	doc, path, err := loadDMSenderAllowDoc()
	if err != nil {
		return err
	}
	for i := range doc.Grants {
		g := &doc.Grants[i]
		if g.Agent == agent && g.Channel == channel && g.Sender == sender {
			g.Revoked = false
			g.By = by
			g.Granted = nowFn()
			return saveDMSenderAllowDoc(doc, path)
		}
	}
	doc.Grants = append(doc.Grants, dmSenderGrant{
		Agent: agent, Channel: channel, Sender: sender, By: by, Granted: nowFn(),
	})
	sort.Slice(doc.Grants, func(i, j int) bool { return doc.Grants[i].Granted.Before(doc.Grants[j].Granted) })
	return saveDMSenderAllowDoc(doc, path)
}

// dmSenderRevokeAccess ends a sender's standing admission. Idempotent:
// revoking an unknown or already-revoked sender reports found=false rather
// than an error.
func dmSenderRevokeAccess(agent, channel, sender string) (bool, error) {
	dmSenderAllowMu.Lock()
	defer dmSenderAllowMu.Unlock()
	doc, path, err := loadDMSenderAllowDoc()
	if err != nil {
		return false, err
	}
	found := false
	for i := range doc.Grants {
		g := &doc.Grants[i]
		if g.Agent == agent && g.Channel == channel && g.Sender == sender && !g.Revoked {
			g.Revoked = true
			found = true
		}
	}
	if !found {
		return false, nil
	}
	return true, saveDMSenderAllowDoc(doc, path)
}

// --- pairing request (the /approve, /deny queue) -----------------------------

// dmPairArgv is the pairing operation's shape for ask.PlanToken: the same
// (agent, channel, sender) always hashes to the same approval, which is what
// keeps a sender resending the same message while pairing is pending from
// raising a second request.
func dmPairArgv(agent, channel, sender string) []string {
	return []string{"meet", "dm", "pair", agent, channel, sender}
}

func dmPairSession(channel, sender string) string {
	return "meet-dm:" + channel + ":" + sender
}

// findDMPairRequest returns the newest approval this front door raised for
// (agent, channel, sender), pending or already decided.
func findDMPairRequest(agent, channel, sender string) (ask.Request, bool, error) {
	all, err := ask.ListApprovals()
	if err != nil {
		return ask.Request{}, false, err
	}
	argv := dmPairArgv(agent, channel, sender)
	session := dmPairSession(channel, sender)
	var best ask.Request
	found := false
	for _, r := range all {
		if r.Approval == nil || r.Approval.Source != ask.SourceMeetDM || r.Approval.Session != session {
			continue
		}
		if !slices.Equal(r.Approval.Argv, argv) {
			continue
		}
		if !found || r.Created.After(best.Created) {
			best, found = r, true
		}
	}
	return best, found, nil
}

// --- the front door itself ---------------------------------------------------

// dmFrontDoorInput is the JSON shape accepted on stdin when no argv text is
// given: {"text": "...", "thread": "..."}.
type dmFrontDoorInput struct {
	Text   string `json:"text"`
	Thread string `json:"thread,omitempty"`
}

// readDMFrontDoorText resolves the message body: the remaining argv after the
// agent, verbatim-joined, or — when none was given — one JSON object read
// from stdin.
func readDMFrontDoorText(stdin io.Reader, argvText []string) (text, thread string, err error) {
	if len(argvText) > 0 {
		return strings.Join(argvText, " "), "", nil
	}
	raw, err := io.ReadAll(bufio.NewReader(stdin))
	if err != nil {
		return "", "", fmt.Errorf("meet: reading dm stdin: %w", err)
	}
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", "", fmt.Errorf("meet: dm needs message text as argv or a JSON object ({\"text\":...}) on stdin")
	}
	var in dmFrontDoorInput
	if err := json.Unmarshal([]byte(trimmed), &in); err != nil {
		return "", "", fmt.Errorf("meet: dm stdin is not the expected JSON ({\"text\":...}): %w", err)
	}
	if strings.TrimSpace(in.Text) == "" {
		return "", "", fmt.Errorf("meet: dm stdin JSON has no text")
	}
	return in.Text, in.Thread, nil
}

// runDMFrontDoor is `bashy meet dm AGENT --channel C --sender S`'s RunE body.
// An unpaired sender is turned back with a pair code and nothing is invoked:
// chat.Invoke is reached only after dmSenderAllowed is true, so a message
// from a sender nobody admitted can never reach — let alone launch — the
// agent.
func runDMFrontDoor(cmd *cobra.Command, agent, channel, sender, thread, text string) error {
	agent = canonAgent(agent)
	if err := routableSeat(agent); err != nil {
		return err
	}
	channel = strings.TrimSpace(channel)
	sender = strings.TrimSpace(sender)
	if channel == "" || sender == "" {
		return fmt.Errorf("meet: dm needs both --channel and --sender for an external message")
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return fmt.Errorf("meet: dm needs message text")
	}
	human := dmChannelHuman(channel, sender)

	body := text
	if thread = strings.TrimSpace(thread); thread != "" {
		body = "[" + thread + "] " + text
	}
	// S3: one room event per message, paired or not — the audit trail an
	// operator reads to decide a pairing, and the evidence that the message
	// arrived at all.
	room.Record(room.EventInbound, human, agent, channel, body)

	allowed, err := dmSenderAllowed(agent, channel, sender)
	if err != nil {
		return err
	}
	if !allowed {
		req, pending, err := findDMPairRequest(agent, channel, sender)
		if err != nil {
			return err
		}
		if !pending {
			req, err = ask.Raise(ask.ApprovalSpec{
				Source:  ask.SourceMeetDM,
				Argv:    dmPairArgv(agent, channel, sender),
				Session: dmPairSession(channel, sender),
				Prompt:  fmt.Sprintf("meet dm: pair sender %s on channel %s to message %s", sender, channel, agent),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"unknown sender %s on channel %s; pair code %s — ask an operator to run `bashy approve allow %s` to admit, or `bashy approve deny %s` to refuse; the message was not delivered\n",
				sender, channel, req.ID, req.ID, req.ID)
			return nil
		}
		d, decided := ask.LoadDecision(req.ID)
		if !decided {
			fmt.Fprintf(cmd.OutOrStdout(),
				"pairing pending for %s on channel %s (code %s) — ask an operator to run `bashy approve allow %s` or `bashy approve deny %s`\n",
				sender, channel, req.ID, req.ID, req.ID)
			return nil
		}
		if !d.Allowed() {
			fmt.Fprintf(cmd.OutOrStdout(), "pairing for %s on channel %s was denied; message not delivered\n", sender, channel)
			return nil
		}
		if err := dmSenderGrantAccess(agent, channel, sender, d.By); err != nil {
			return err
		}
	}

	if err := appendDMFrontDoorEvent(agent, human, relayDMEvent{Speaker: human, Role: "human", Text: body, TS: nowFn()}); err != nil {
		return err
	}

	readOnly, allowUnsafe := turnAuthority()
	res, invokeErr := invokeRelayDM(cmd.Context(), chat.Options{
		Agent: agent, Instruction: text, ReadOnly: readOnly, AllowUnsafe: allowUnsafe,
	}, nil)
	reply := strings.TrimSpace(normalizeTurnText(res.Output))
	status := "ok"
	if invokeErr != nil {
		status = "error"
		if reply == "" {
			reply = invokeErr.Error()
		}
	}
	if reply == "" {
		reply = "(agent returned no text)"
	}
	if err := appendDMFrontDoorEvent(agent, human, relayDMEvent{Speaker: agent, Role: "assistant", Text: reply, Status: status, TS: nowFn()}); err != nil {
		return err
	}
	fmt.Fprintln(cmd.OutOrStdout(), reply)
	if invokeErr != nil {
		return fmt.Errorf("meet: %s: %w", agent, invokeErr)
	}
	return nil
}

// appendDMFrontDoorEvent writes into the shared per-(channel,sender) DM room,
// under the same lock relay_dm.go's handlers use for this agent.
func appendDMFrontDoorEvent(agent, human string, event relayDMEvent) error {
	lock := relayDMLock(agent)
	lock.Lock()
	defer lock.Unlock()
	st, err := dmRoomFor(agent, human)
	if err != nil {
		return err
	}
	ts := event.TS
	if ts.IsZero() {
		ts = nowFn()
	}
	kind, to := "message", ""
	if event.Role == "human" {
		kind, to = "human", canonAgent(agent)
	}
	return AppendEvent(st.ID, Event{
		Round: st.Round, Speaker: event.Speaker, Role: string(RoleParticipant),
		Kind: kind, To: to, Text: event.Text, Status: event.Status, TS: ts,
	})
}

// runDMFrontDoorRevoke is `bashy meet dm AGENT --channel C --sender S
// --revoke`: it ends a sender's standing admission without touching the
// transcript or invoking the agent.
func runDMFrontDoorRevoke(cmd *cobra.Command, agent, channel, sender string) error {
	agent = canonAgent(agent)
	channel, sender = strings.TrimSpace(channel), strings.TrimSpace(sender)
	if channel == "" || sender == "" {
		return fmt.Errorf("meet: dm --revoke needs both --channel and --sender")
	}
	found, err := dmSenderRevokeAccess(agent, channel, sender)
	if err != nil {
		return err
	}
	if !found {
		fmt.Fprintf(cmd.OutOrStdout(), "%s on channel %s was not paired with %s\n", sender, channel, agent)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "revoked %s on channel %s for %s\n", sender, channel, agent)
	return nil
}
