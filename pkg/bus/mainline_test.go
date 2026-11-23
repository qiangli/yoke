// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package bus

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

func testOpenMainLineInstance(t *testing.T, store *fleet.InstanceStore, label, session string, mailFrom int64) fleet.Instance {
	t.Helper()
	inst, err := store.Open(fleet.Family{
		Name: "claude", Display: MainLineBase(label), Policy: fleet.PolicySingle, Bindings: []string{"claude:sonnet4.6"},
	}, fleet.OpenOptions{Label: label, MailFrom: mailFrom})
	if err != nil {
		t.Fatalf("open %q: %v", label, err)
	}
	if session != "" {
		if err := room.Join(room.Card{
			ID:           inst.ClaimID(),
			Nick:         label,
			Tool:         "claude",
			Binding:      "claude:sonnet4.6",
			Mode:         "interactive",
			PID:          os.Getpid(),
			OwnerPID:     os.Getpid(),
			SessionClaim: HashSessionClaim(session),
			Principal:    principal.InstanceURN(inst.UUID),
		}); err != nil {
			t.Fatalf("room.Join %q: %v", label, err)
		}
	}
	return inst
}

func runMBCmd(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewMessageBoardCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func TestMainLine_ExtensionIsolationAndReusedExtensionStartsEmpty(t *testing.T) {
	boardInTempHome(t)
	IsMainLineTool = func(name string) bool { return strings.EqualFold(name, "claude") }
	t.Cleanup(func() { IsMainLineTool = nil })
	t.Setenv("BASHY_SESSION_CLAIM", "")

	store := InstanceStoreFn()
	i1 := testOpenMainLineInstance(t, store, "claude", "sess-1", 0)
	i2 := testOpenMainLineInstance(t, store, "claude+2", "sess-2", 0)
	i3 := testOpenMainLineInstance(t, store, "claude+3", "sess-3", 0)

	if _, err := Send(SendRequest{From: "operator", To: "claude+2", Body: "only for plus2"}); err != nil {
		t.Fatalf("send to claude+2: %v", err)
	}
	if _, err := Send(SendRequest{From: "operator", To: "claude+3", Body: "only for plus3"}); err != nil {
		t.Fatalf("send to claude+3: %v", err)
	}

	// Peek stays observational and isolates each extension's direct mail.
	out1, _, err := runMBCmd(t, "--as", "claude", "--peek")
	if err != nil {
		t.Fatalf("peek claude: %v", err)
	}
	if strings.Contains(out1, "only for plus2") || strings.Contains(out1, "only for plus3") {
		t.Fatalf("claude saw extension direct mail:\n%s", out1)
	}

	out2, _, err := runMBCmd(t, "--as", "claude+2", "--peek")
	if err != nil || !strings.Contains(out2, "only for plus2") || strings.Contains(out2, "only for plus3") {
		t.Fatalf("claude+2 peek mismatch (err=%v):\n%s", err, out2)
	}
	out3, _, err := runMBCmd(t, "--as", "claude+3", "--peek")
	if err != nil || !strings.Contains(out3, "only for plus3") || strings.Contains(out3, "only for plus2") {
		t.Fatalf("claude+3 peek mismatch (err=%v):\n%s", err, out3)
	}

	// Second unread direct message for claude+2 before retiring i2.
	if _, err := Send(SendRequest{From: "operator", To: "claude+2", Body: "old unread for dead plus2"}); err != nil {
		t.Fatalf("second send to claude+2: %v", err)
	}
	if _, err := store.Retire(i2.UUID, InstanceMail); err != nil {
		t.Fatalf("retire i2: %v", err)
	}
	room.Leave(i2.ClaimID())

	// Reused +2 opens with MailFrom = head + 1 and must start completely empty.
	head := BoardHeadSeq()
	i2New := testOpenMainLineInstance(t, store, "claude+2", "sess-2b", head+1)
	if i2New.UUID == i2.UUID || i1.UUID == i3.UUID {
		t.Fatalf("expected distinct instance UUIDs")
	}
	out2New, _, err := runMBCmd(t, "--as", "claude+2")
	if err != nil {
		t.Fatalf("reused claude+2 read: %v", err)
	}
	if strings.Contains(out2New, "only for plus2") || strings.Contains(out2New, "old unread for dead plus2") {
		t.Fatalf("reused claude+2 saw old generation's mail:\n%s", out2New)
	}
}

func TestMainLine_HuntGroupWaitsWithoutLiveSessionAndClaimsOnceConcurrently(t *testing.T) {
	boardInTempHome(t)
	IsMainLineTool = func(name string) bool { return strings.EqualFold(name, "claude") }
	t.Cleanup(func() { IsMainLineTool = nil })
	t.Setenv("BASHY_SESSION_CLAIM", "")

	// Send to main line while nobody is live.
	res, err := Send(SendRequest{From: "operator", To: "claude", Body: "hunt-group task 1"})
	if err != nil {
		t.Fatalf("send to claude main line: %v", err)
	}

	posts, err := Posts()
	if err != nil || len(posts) != 1 || !posts[0].IsHuntGroup() {
		t.Fatalf("expected hunt-group post, got %+v (err=%v)", posts, err)
	}

	// A reader with NO live room card must not claim the hunt-group post.
	store := InstanceStoreFn()
	ghost := testOpenMainLineInstance(t, store, "claude+9", "", BoardHeadSeq()+1)
	ghostOut, _, err := runMBCmd(t, "--as", "claude+9")
	if err != nil {
		t.Fatalf("non-live claude+9 read failed: %v", err)
	}
	if strings.Contains(ghostOut, "hunt-group task 1") {
		t.Fatalf("non-live reader claimed waiting hunt-group post:\n%s", ghostOut)
	}
	if holder := ClaimHolder(posts[0].Seq); holder != "" {
		t.Fatalf("ClaimHolder = %q after non-live read; want empty", holder)
	}
	_ = ghost

	// Open 4 live instances across claude + extensions and observe first with --peek.
	var members []fleet.Instance
	for idx, label := range []string{"claude", "claude+2", "claude+3", "claude+4"} {
		members = append(members, testOpenMainLineInstance(t, store, label, fmt.Sprintf("sess-%d", idx), BoardHeadSeq()+1))
	}
	peekOut, _, err := runMBCmd(t, "--as", "claude+2", "--peek")
	if err != nil || !strings.Contains(peekOut, "hunt-group task 1") {
		t.Fatalf("live --peek missed waiting hunt-group post (err=%v):\n%s", err, peekOut)
	}
	if holder := ClaimHolder(posts[0].Seq); holder != "" {
		t.Fatalf("--peek claimed hunt-group post: %q", holder)
	}

	// Concurrent live readers race to read the waiting hunt-group message.
	var winners atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	outputs := make([]string, len(members))
	errs := make([]error, len(members))
	for idx, inst := range members {
		wg.Add(1)
		go func(i int, target fleet.Instance) {
			defer wg.Done()
			<-start
			out, _, err := runMBCmd(t, "--as", target.MailAddress())
			outputs[i] = out
			errs[i] = err
			if strings.Contains(out, "hunt-group task 1") {
				winners.Add(1)
			}
		}(idx, inst)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent reader %d failed: %v", i, err)
		}
	}
	if got := winners.Load(); got != 1 {
		t.Fatalf("concurrent hunt-group winners = %d, want exactly 1; outputs=%q", got, outputs)
	}
	if holder := ClaimHolder(res.Seq); holder == "" {
		t.Fatalf("expected atomic ClaimHolder recorded on seq %d, got empty", res.Seq)
	}
}

func TestMainLine_SelfSendCallerIDAndCrossGenerationRefusal(t *testing.T) {
	boardInTempHome(t)
	IsMainLineTool = func(name string) bool { return strings.EqualFold(name, "claude") }
	t.Cleanup(func() { IsMainLineTool = nil })
	DetectHarness = func() (string, bool) { return "claude", true }
	t.Cleanup(func() { DetectHarness = nil })
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_INSTANCE_UUID", "")
	t.Setenv("BASHY_SESSION_CLAIM", "")

	store := InstanceStoreFn()
	i2 := testOpenMainLineInstance(t, store, "claude+2", "sess-plus2", 0)
	i3 := testOpenMainLineInstance(t, store, "claude+3", "sess-plus3", 0)

	// Stamped as i2 with matching session claim -> `mb send` attributes as claude+2.
	t.Setenv("BASHY_PRINCIPAL", principal.InstanceURN(i2.UUID))
	t.Setenv("BASHY_INSTANCE", i2.UUID)
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("sess-plus2"))

	if _, _, err := runMBCmd(t, "send", "claude+3", "hello from self"); err != nil {
		t.Fatalf("self mb send failed: %v", err)
	}
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "hello via --as plus2"); err != nil {
		t.Fatalf("--as claude+2 mb send failed: %v", err)
	}

	posts, err := Posts()
	if err != nil || len(posts) != 2 {
		t.Fatalf("posts = %+v (err=%v), want 2", posts, err)
	}
	for _, p := range posts {
		if p.From != "claude+2" || p.FromParty == nil || p.FromParty.UUID != i2.UUID {
			t.Fatalf("wrong caller ID attribution on post %+v; want From=claude+2 UUID=%s", p, i2.UUID)
		}
	}

	// Cross-generation / cross-extension refusal: i2 cannot author as claude+3 or its UUID.
	if _, _, err := runMBCmd(t, "send", "--as", "claude+3", "claude+3", "spoof"); err == nil {
		t.Fatal("expected refusal authoring as claude+3 from claude+2 session")
	}
	if _, _, err := runMBCmd(t, "send", "--as", i3.UUID, "claude+3", "spoof uuid"); err == nil {
		t.Fatal("expected refusal authoring as i3 UUID from claude+2 session")
	}

	// Retire i2 and reuse claude+2: stale generation UUID in BASHY_PRINCIPAL must refuse claude+2!
	if _, err := store.Retire(i2.UUID, InstanceMail); err != nil {
		t.Fatal(err)
	}
	i2New := testOpenMainLineInstance(t, store, "claude+2", "sess-plus2-new", BoardHeadSeq()+1)
	_ = i2New
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "stale gen spoof"); err == nil {
		t.Fatal("expected retired i2 session to be refused authoring as new generation claude+2")
	}

	// External harness with matching BASHY_SESSION_CLAIM for i3 and no BASHY_PRINCIPAL can use --as claude+3.
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("sess-plus3"))
	if _, _, err := runMBCmd(t, "send", "--as", "claude+3", "claude+2", "from external plus3"); err != nil {
		t.Fatalf("authorized external --as claude+3 failed: %v", err)
	}
	// Wrong session claim must refuse external --as claude+3.
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("wrong-session"))
	if _, _, err := runMBCmd(t, "send", "--as", "claude+3", "claude+2", "hijack plus3"); err == nil {
		t.Fatal("expected refusal for wrong session claim authoring as claude+3")
	}
}

func TestMainLine_UUIDMatchedCardFencingAndInheritedSessionClaim(t *testing.T) {
	boardInTempHome(t)
	IsMainLineTool = func(name string) bool { return strings.EqualFold(name, "claude") }
	t.Cleanup(func() { IsMainLineTool = nil })
	DetectHarness = func() (string, bool) { return "claude", true }
	t.Cleanup(func() { DetectHarness = nil })
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_INSTANCE_UUID", "")
	t.Setenv("BASHY_SESSION_CLAIM", "")

	store := InstanceStoreFn()
	// Generation 1: claude+2 with live card, then retired leaving stale label card
	// and stale BASHY_INSTANCE trace.
	gen1 := testOpenMainLineInstance(t, store, "claude+2", "sess-gen1", 0)
	if _, err := store.Retire(gen1.UUID, InstanceMail); err != nil {
		t.Fatal(err)
	}
	room.Leave(gen1.ClaimID())

	// Join a label-only card with NO UUID and a label card carrying gen1's stale UUID.
	if err := room.Join(room.Card{
		ID: room.AgentClaimID("claude+2"), Nick: "claude+2", Tool: "claude",
		PID: os.Getpid(), OwnerPID: os.Getpid(),
		SessionClaim: HashSessionClaim("label-only-no-uuid"),
		Principal:    "operator",
	}); err != nil {
		t.Fatal(err)
	}
	if err := room.Join(room.Card{
		ID: "claude+2", Nick: "claude+2", Tool: "claude",
		PID: os.Getpid(), OwnerPID: os.Getpid(),
		SessionClaim: HashSessionClaim("sess-gen1"),
		Principal:    principal.InstanceURN(gen1.UUID),
	}); err != nil {
		t.Fatal(err)
	}

	// Retired gen1 UUID and retired extension label must NOT be considered live.
	if ReaderHasLiveSession(gen1.MailAddress(), "claude") {
		t.Fatal("retired UUID satisfied ReaderHasLiveSession via label fallback")
	}
	if ReaderHasLiveSession("claude+2", "claude") {
		t.Fatal("retired/stale label satisfied ReaderHasLiveSession via label-only card")
	}

	// Generation 2 opens claude+2 with NO instance:<gen2> card yet:
	// neither the label-only card nor gen1's stale-UUID label card may satisfy gen2.
	gen2 := testOpenMainLineInstance(t, store, "claude+2", "", BoardHeadSeq()+1)
	if ReaderHasLiveSession(gen2.MailAddress(), "claude") || ReaderHasLiveSession("claude+2", "claude") {
		t.Fatal("gen2 appeared live from label-only or stale-UUID room card")
	}

	// Stale generation trace in BASHY_INSTANCE (even with empty BASHY_PRINCIPAL)
	// must be refused on both bare send and --as claude+2.
	gen3 := testOpenMainLineInstance(t, store, "claude+3", "sess-gen3", BoardHeadSeq()+1)
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", gen1.UUID)
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("sess-gen1"))
	if _, _, err := runMBCmd(t, "send", "claude+3", "from retired gen1"); err == nil {
		t.Fatal("expected refusal for stale BASHY_INSTANCE on bare mb send")
	}
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "spoof gen2"); err == nil {
		t.Fatal("expected refusal for stale BASHY_INSTANCE authoring as reused claude+2")
	}

	// Old generation with BASHY_AGENT=claude+2 and stale BASHY_INSTANCE=gen1.UUID
	// must also be refused even before gen2 joins a room card.
	t.Setenv("BASHY_AGENT", "claude+2")
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "stale trace + BASHY_AGENT"); err == nil {
		t.Fatal("expected refusal when stale BASHY_INSTANCE accompanies BASHY_AGENT=claude+2")
	}
	t.Setenv("BASHY_AGENT", "")

	// Now gen2 joins a UUID-matched card under its label ID (with foreign OwnerPID
	// to prove inherited BASHY_SESSION_CLAIM digest protocol without PID ancestry).
	room.Leave(room.AgentClaimID("claude+2"))
	room.Leave("claude+2")
	foreignPID := foreignLivePID(t)
	if err := room.Join(room.Card{
		ID: "claude+2", Nick: "claude+2", Tool: "claude",
		PID: foreignPID, OwnerPID: foreignPID,
		SessionClaim: HashSessionClaim("sess-gen2"),
		Principal:    principal.InstanceURN(gen2.UUID),
	}); err != nil {
		t.Fatal(err)
	}
	if !ReaderHasLiveSession(gen2.MailAddress(), "claude") || !ReaderHasLiveSession("claude+2", "claude") {
		t.Fatal("gen2 with UUID-matched live label card should satisfy ReaderHasLiveSession")
	}

	// Inherited BASHY_SESSION_CLAIM digest succeeds for gen2 even with foreign OwnerPID.
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("sess-gen2"))
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "gen2 inherited digest"); err != nil {
		t.Fatalf("inherited BASHY_SESSION_CLAIM digest for UUID-matched gen2 failed: %v", err)
	}

	// Competing --as refusal: another live extension session (claude+3) and a
	// wrong-digest session must both be refused when passing --as claude+2 or gen2 UUID.
	t.Setenv("BASHY_PRINCIPAL", principal.InstanceURN(gen3.UUID))
	t.Setenv("BASHY_INSTANCE", gen3.UUID)
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("sess-gen3"))
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "competing gen3"); err == nil {
		t.Fatal("expected competing --as claude+2 refusal from claude+3 session")
	}
	if _, _, err := runMBCmd(t, "send", "--as", gen2.UUID, "claude+3", "competing gen3 by uuid"); err == nil {
		t.Fatal("expected competing --as <gen2.UUID> refusal from claude+3 session")
	}
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_INSTANCE", "")
	t.Setenv("BASHY_SESSION_CLAIM", HashSessionClaim("competing-wrong-digest"))
	if _, _, err := runMBCmd(t, "send", "--as", "claude+2", "claude+3", "competing digest"); err == nil {
		t.Fatal("expected competing digest --as claude+2 refusal")
	}
}
