package bus

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func boardInTempHome(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	// The board reads subscriptions too (declared concerns route posts), so
	// the room store must be a temp dir or a test would read the host's.
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("USER", "tester")
	// ResolveSendTarget's resolver fallback reads the fleet catalog and the
	// observation stores; point all of them at empty temp dirs so no test
	// resolves (or fails to resolve) against the operator's real host state.
	t.Setenv("BASHY_FLEET_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_AGENTS_DIR", "")
	t.Setenv("BASHY_PEOPLE_DIR", "")
	t.Setenv("BASHY_AGENTS_PATH", "")
	t.Setenv("BASHY_PEOPLE_PATH", "")
}

// THE BUG THIS STORE FIXES. Posts are addressed to the FLEET NAME, but a
// reader's environment carries something else — a bashy-launched agent has
// BASHY_PRINCIPAL=dhnt:agent/<Nick>. Resolving both to one name is what lets a
// bare `bashy mb` work instead of every agent being told its own identity.
func TestBoardIdentity_ResolvesAPrincipalToTheFleetName(t *testing.T) {
	boardInTempHome(t)
	FleetResolveName = func(s string) string {
		if s == "Omar" {
			return "codex-gpt5.6-sol"
		}
		return ""
	}
	t.Cleanup(func() { FleetResolveName = nil })

	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/Omar")
	if got, err := BoardIdentity(""); err != nil || got != "codex-gpt5.6-sol" {
		t.Fatalf("identity = %q (%v), want the fleet name", got, err)
	}
	// An explicit --as still wins, and also resolves.
	if got, err := BoardIdentity("Omar"); err != nil || got != "codex-gpt5.6-sol" {
		t.Fatalf("--as identity = %q (%v)", got, err)
	}
	// A human at a terminal is a legitimate participant under their login name,
	// not an agent to be resolved into one.
	t.Setenv("BASHY_PRINCIPAL", "")
	if got, err := BoardIdentity(""); err != nil || got != "tester" {
		t.Fatalf("a non-agent must be used as itself, got %q (%v)", got, err)
	}
}

// THE MISATTRIBUTION BUG. An agent in a raw TUI has no BASHY_PRINCIPAL and
// inherits the operator's environment, so the login-name fallback signed its
// posts — and advanced its cursor, and took its claims — as the operator.
//
// Measured on a live board 2026-08-03: six of eight posts read `from: qiangli`,
// spanning the operator and two different agents, and one reply arrived
// addressed FROM its own recipient. Attribution is the board's single
// guarantee, so a caller that is demonstrably an agent and resolves to nothing
// must be refused rather than signed for.
func TestBoardIdentity_RefusesToSignAnAgentWithTheLoginName(t *testing.T) {
	boardInTempHome(t)
	DetectHarness = func() (string, bool) { return "codex", true }
	t.Cleanup(func() { DetectHarness = nil })

	got, err := BoardIdentity("")
	if !errors.Is(err, ErrUnattributed) {
		t.Fatalf("an unattributed agent got identity %q, err %v — want a refusal", got, err)
	}
	// The refusal has to be actionable, or it just moves the failure.
	for _, want := range []string{"--as", "codex", "tester"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not mention %q: %v", want, err)
		}
	}
	// --as is the escape hatch, for an agent naming itself AND for a human who
	// means to speak as themselves from inside an agent session.
	if got, err := BoardIdentity("tester"); err != nil || got != "tester" {
		t.Fatalf("explicit --as under a harness = %q (%v)", got, err)
	}
	// A bashy-launched agent resolves and is never refused.
	FleetResolveName = func(string) string { return "codex-gpt5.6-sol" }
	t.Cleanup(func() { FleetResolveName = nil })
	t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/Omar")
	if got, err := BoardIdentity(""); err != nil || got != "codex-gpt5.6-sol" {
		t.Fatalf("a principal-carrying agent = %q (%v)", got, err)
	}
}

// A nil DetectHarness must not refuse: pkg/bus is importable by hosts with no
// catalog, and breaking the board on a host that simply cannot answer the
// question would be worse than the misattribution it prevents. The other half
// of this contract — that bashy actually WIRES the hook — is pinned in
// bashy's internal/agentos, because a seam nobody connects is how this class
// of bug survives a fix.
func TestBoardIdentity_UnwiredHostKeepsTheLoginName(t *testing.T) {
	boardInTempHome(t)
	DetectHarness = nil
	if got, err := BoardIdentity(""); err != nil || got != "tester" {
		t.Fatalf("unwired host = %q (%v), want the login name", got, err)
	}
}

// PUBLIC BY CONSTRUCTION: addressing says who should ACT, never who may read.
func TestBoard_EveryoneCanReadEverything(t *testing.T) {
	boardInTempHome(t)
	for _, p := range []Post{
		{From: "a", Body: "to everyone"},
		{From: "a", To: "agent-x", Body: "for x"},
		{From: "a", To: "agent-y", Body: "for y"},
	} {
		if err := PostMessage(p); err != nil {
			t.Fatal(err)
		}
	}
	// A reader's default view is what it should act on...
	directed, other, _, err := Unseen("agent-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 1 || len(other) != 1 { // its own + the broadcast
		t.Fatalf("agent-x sees %d directed / %d other, want 1/1", len(directed), len(other))
	}
	// ...but the whole board is readable by anyone. That is not an escalation:
	// it is the point of a public board.
	all, err := Posts()
	if err != nil || len(all) != 3 {
		t.Fatalf("Posts() = %d (err %v), want 3", len(all), err)
	}
}

// A first-time reader sees the WHOLE board, not nothing — the opposite of the
// private-inbox rule that opens a new mailbox at the head. Public means the
// history was always yours to read.
func TestBoard_FirstReadSeesTheHistory(t *testing.T) {
	boardInTempHome(t)
	for range 3 {
		if err := PostMessage(Post{From: "a", Body: "old news"}); err != nil {
			t.Fatal(err)
		}
	}
	_, other, _, err := Unseen("brand-new-reader", 0)
	if err != nil || len(other) != 3 {
		t.Fatalf("first read = %d (err %v), want the whole board", len(other), err)
	}
}

func TestBoard_CursorAdvancesAndNeverGoesBackwards(t *testing.T) {
	boardInTempHome(t)
	for range 2 {
		if err := PostMessage(Post{From: "a", Body: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := MarkSeen("r", 2); err != nil {
		t.Fatal(err)
	}
	if d, o, _, _ := Unseen("r", 0); len(d)+len(o) != 0 {
		t.Fatalf("after marking, %d unseen", len(d)+len(o))
	}
	// Re-reading an older view must not un-see what was already shown.
	if err := MarkSeen("r", 1); err != nil {
		t.Fatal(err)
	}
	if SeenSeq("r") != 2 {
		t.Fatalf("cursor went backwards to %d", SeenSeq("r"))
	}
}

// An unattributable post is worse than none: nobody can ask the sender what
// they meant.
func TestBoard_PostNeedsASender(t *testing.T) {
	boardInTempHome(t)
	if err := PostMessage(Post{Body: "who said this?"}); err == nil {
		t.Fatal("a post with no sender must be refused")
	}
}

func TestBoard_AbsentBoardIsEmptyNotAnError(t *testing.T) {
	t.Setenv("BASHY_MB_DIR", "/nonexistent-xyz")
	posts, err := Posts()
	if err != nil {
		t.Fatalf("an absent board is a state, not a failure: %v", err)
	}
	if len(posts) != 0 {
		t.Fatalf("got %d posts", len(posts))
	}
	_ = os.Getenv("HOME")
}

// A SELECTOR IS STORED, NOT EXPANDED. Expanding made the board grow with the
// size of the audience — `--band 4` wrote eight identical posts — so --all
// became unreadable and every reader's scan got longer for a line that
// concerned one of them.
func TestBoard_SelectorIsOnePostAndResolvesAtReadTime(t *testing.T) {
	boardInTempHome(t)
	FleetSelect = func(a Audience) ([]string, error) {
		if a.Band == 4 {
			return []string{"claude-opus5", "codex-gpt5.6-sol"}, nil
		}
		return nil, nil
	}
	t.Cleanup(func() { FleetSelect = nil })

	if err := PostMessage(Post{From: "steward", Audience: &Audience{Band: 4}, Body: "L4 only"}); err != nil {
		t.Fatal(err)
	}
	all, _ := Posts()
	if len(all) != 1 {
		t.Fatalf("a selector post must be ONE record, got %d", len(all))
	}
	// In the audience → sees it.
	if _, other, _, _ := Unseen("claude-opus5", 0); len(other) != 1 {
		t.Fatal("an agent in the audience must see a selector post")
	}
	// Outside it → does not.
	if _, other, _, _ := Unseen("agy-gemini3.1", 0); len(other) != 0 {
		t.Fatal("an agent outside the audience must not see it in its default view")
	}
	// But the board is still public.
	if posts, _ := Posts(); len(posts) != 1 {
		t.Fatal("the post must remain readable by anyone via --all")
	}
}

// DIRECTED IS NEVER CAPPED; the rest is, and the overflow is REPORTED. A cap
// that stays quiet is a silent drop, and a reader cannot tell "nothing else"
// from "twelve more" unless it is told.
func TestBoard_DirectedUncappedOtherCappedWithReportedOverflow(t *testing.T) {
	boardInTempHome(t)
	for range 9 {
		if err := PostMessage(Post{From: "a", Body: "fyi"}); err != nil {
			t.Fatal(err)
		}
	}
	for range 7 {
		if err := PostMessage(Post{From: "a", To: "me", Body: "do this"}); err != nil {
			t.Fatal(err)
		}
	}
	directed, other, older, err := Unseen("me", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(directed) != 7 {
		t.Fatalf("directed = %d; an obligation must never be truncated", len(directed))
	}
	if len(other) != 5 {
		t.Fatalf("other = %d, want the cap of 5", len(other))
	}
	if older != 4 {
		t.Fatalf("older = %d, want 4 reported as hidden", older)
	}
	// The newest are kept, not the oldest.
	if other[len(other)-1].Seq != 9 {
		t.Fatalf("the cap must keep the NEWEST, last seq = %d", other[len(other)-1].Seq)
	}
}

// MODE ANY — work offered to a pool. The first reader claims it and the rest
// never see it; two agents must not do the same job.
func TestBoard_AnyModeIsClaimedByTheFirstReader(t *testing.T) {
	boardInTempHome(t)
	FleetSelect = func(a Audience) ([]string, error) {
		return []string{"a1", "a2", "a3"}, nil
	}
	t.Cleanup(func() { FleetSelect = nil })

	if err := PostMessage(Post{
		From: "steward", Audience: &Audience{Band: 3}, Mode: ModeAny, Body: "take P0-1",
	}); err != nil {
		t.Fatal(err)
	}
	// Everyone in the pool sees it while unclaimed.
	for _, who := range []string{"a1", "a2"} {
		if _, other, _, _ := Unseen(who, 0); len(other) != 1 {
			t.Fatalf("%s should see an unclaimed offer", who)
		}
	}
	holder, granted := ClaimPost(1, "a1")
	if !granted || holder != "a1" {
		t.Fatalf("first claim must be granted, got %q/%v", holder, granted)
	}
	// A second claimant loses and is TOLD who holds it.
	holder, granted = ClaimPost(1, "a2")
	if granted || holder != "a1" {
		t.Fatalf("second claim must lose to a1, got %q/%v", holder, granted)
	}
	// And the offer is gone from everyone else's view.
	if _, other, _, _ := Unseen("a2", 0); len(other) != 0 {
		t.Fatal("a claimed offer must not be shown to others")
	}
	if _, other, _, _ := Unseen("a1", 0); len(other) != 1 {
		t.Fatal("the holder must still see what it took")
	}
}

// MODE ALL — an announcement. Everybody sees it and views are counted, so the
// sender can ask whether it actually reached the group.
func TestBoard_AllModeCountsDistinctViewers(t *testing.T) {
	boardInTempHome(t)
	FleetSelect = func(a Audience) ([]string, error) { return []string{"a1", "a2", "a3"}, nil }
	t.Cleanup(func() { FleetSelect = nil })

	if err := PostMessage(Post{
		From: "steward", Audience: &Audience{Band: 4}, Mode: ModeAll, Body: "quota exhausted",
	}); err != nil {
		t.Fatal(err)
	}
	for _, who := range []string{"a1", "a2", "a1"} { // a1 twice
		if err := RecordView(1, who); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(Viewers(1)); got != 2 {
		t.Fatalf("viewers = %d, want 2 distinct (a re-read must not inflate it)", got)
	}
	if n := AudienceSize(Audience{Band: 4}); n != 3 {
		t.Fatalf("audience size = %d, want 3 for the 'N of M' line", n)
	}
	// An announcement is never consumed: every member still sees it.
	for _, who := range []string{"a1", "a2", "a3"} {
		if _, other, _, _ := Unseen(who, 0); len(other) != 1 {
			t.Fatalf("%s must still see an announcement", who)
		}
	}
}

// AUDIENCE MEMBERSHIP IS AN UNCAPPED TIER — the sprint #139 payoff.
//
// A group post is addressed to a reader because they are DOING something
// (managing a sprint), not because they happened to declare a concern. A
// member who must first subscribe to see their own mail in full has an
// obligation trimmed by an arbitrary -n, which is the directed-tier rule
// applied to a group: never truncate what somebody is supposed to act on.
func TestBoard_AudienceMemberSeesGroupPostUncapped(t *testing.T) {
	boardInTempHome(t)
	FleetSelect = func(Audience) ([]string, error) {
		return []string{"claude-opus5"}, nil
	}
	t.Cleanup(func() { FleetSelect = nil })

	// The group post is the OLDEST thing on the board: if it were capped like
	// an ordinary broadcast, the newest-five rule would trim it first.
	if err := PostMessage(Post{From: "steward", Audience: &Audience{Role: "conductor"}, Body: "sprint moved"}); err != nil {
		t.Fatal(err)
	}
	for range 9 {
		if err := PostMessage(Post{From: "a", Body: "fyi"}); err != nil {
			t.Fatal(err)
		}
	}
	_, other, older, err := Unseen("claude-opus5", 5)
	if err != nil {
		t.Fatal(err)
	}
	if older != 4 {
		t.Fatalf("older = %d, want 4 hidden broadcasts — only the broadcasts may be capped", older)
	}
	if len(other) != 6 || other[0].Seq != 1 {
		t.Fatalf("other = %d posts starting at seq %d; the member must see the group post (seq 1) uncapped alongside the capped 5", len(other), other[0].Seq)
	}
}

// Lease membership changes even when the board itself has not changed. Each
// read must re-resolve it, including empty and failed previous resolutions.
func TestBoard_AudienceRefreshesBetweenReads(t *testing.T) {
	for _, entry := range []string{"unseen", "membership", "post"} {
		t.Run(entry, func(t *testing.T) {
			boardInTempHome(t)
			var names []string
			var resolveErr error
			oldSelect := FleetSelect
			FleetSelect = func(Audience) ([]string, error) { return names, resolveErr }
			t.Cleanup(func() { FleetSelect = oldSelect })
			p := Post{From: "tester", Audience: &Audience{Role: "conductor"}, Body: "lease notice"}
			if err := PostMessage(p); err != nil {
				t.Fatal(err)
			}
			check := func(reader string, want bool) {
				t.Helper()
				var got bool
				switch entry {
				case "membership":
					got = InAudience(*p.Audience, reader)
				case "post":
					got = p.ForReader(reader)
				default:
					directed, other, older, err := Unseen(reader, 1)
					if err != nil {
						t.Fatal(err)
					}
					if len(directed) != 0 || older != 0 {
						t.Fatalf("directed=%d older=%d", len(directed), older)
					}
					got = len(other) == 1
				}
				if got != want {
					t.Errorf("reader %q with roster %v and error %v: got %v, want %v", reader, names, resolveErr, got, want)
				}
			}
			check("old", false) // initially no live leases
			names = []string{" Old "}
			check("OLD", true) // a manager seats in the same process
			names = []string{"new"}
			check("old", false) // handoff removes the previous owner
			check("new", true)
			names = nil
			check("new", false) // expired leases leave the live roster
			resolveErr = errors.New("roster unavailable")
			check("new", false)
			names, resolveErr = []string{"new"}, nil
			check("new", true) // a later read retries failed or empty resolution
		})
	}
}

func TestBoard_AudienceResolvedOncePerRead(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "members", true: "unavailable"}[failed], func(t *testing.T) {
			boardInTempHome(t)
			calls := map[Audience]int{}
			oldSelect := FleetSelect
			FleetSelect = func(a Audience) ([]string, error) {
				calls[a]++
				if failed {
					return nil, errors.New("roster unavailable")
				}
				return []string{"reader"}, nil
			}
			t.Cleanup(func() { FleetSelect = oldSelect })
			selectors := []Audience{{Role: "conductor"}, {Band: 4}}
			for _, aud := range selectors {
				for range 7 {
					if err := PostMessage(Post{From: "tester", Audience: &aud, Body: "notice"}); err != nil {
						t.Fatal(err)
					}
				}
			}
			clear(calls)
			for read := 1; read <= 2; read++ {
				_, other, older, err := Unseen("reader", 1)
				if err != nil {
					t.Fatal(err)
				}
				want := 14
				if failed {
					want = 0
				}
				if len(other) != want || older != 0 {
					t.Fatalf("posts=%d older=%d, want %d/0", len(other), older, want)
				}
				for _, aud := range selectors {
					if calls[aud] != read {
						t.Errorf("read %d: selector %+v resolved %d times, want %d", read, aud, calls[aud], read)
					}
				}
			}
		})
	}
}

func TestBoard_AudienceConcurrentReads(t *testing.T) {
	boardInTempHome(t)
	var calls atomic.Int32
	oldSelect := FleetSelect
	FleetSelect = func(Audience) ([]string, error) { calls.Add(1); return []string{"reader"}, nil }
	t.Cleanup(func() { FleetSelect = oldSelect })
	for range 7 {
		if err := PostMessage(Post{From: "tester", Audience: &Audience{Role: "conductor"}, Body: "notice"}); err != nil {
			t.Fatal(err)
		}
	}
	calls.Store(0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, other, _, err := Unseen("reader", 1)
			if err != nil || len(other) != 7 {
				t.Errorf("posts=%d error=%v", len(other), err)
			}
		})
	}
	wg.Wait()
	if got := calls.Load(); got != 8 {
		t.Fatalf("resolver calls=%d, want one per concurrent read (8)", got)
	}
}

func TestMessageBoard_AudienceSnapshotSharedWithReceipts(t *testing.T) {
	for _, mode := range []string{"read", "peek", "history"} {
		t.Run(mode, func(t *testing.T) {
			boardInTempHome(t)
			calls := 0
			names := []string{"first"}
			oldSelect := FleetSelect
			FleetSelect = func(Audience) ([]string, error) { calls++; return names, nil }
			t.Cleanup(func() { FleetSelect = oldSelect })
			for range 7 {
				if err := PostMessage(Post{From: "tester", Audience: &Audience{Role: "conductor"}, Body: "notice"}); err != nil {
					t.Fatal(err)
				}
			}
			calls = 0
			for i, reader := range []string{"first", "second"} {
				args := []string{"--as", reader}
				if mode != "read" {
					args = append(args, "--"+mode)
				}
				out, _, err := runMessageBoard(t, context.Background(), args...)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(out, "notice") != 7 {
					t.Fatalf("lost addressed posts:\n%s", out)
				}
				denominator := " of 1)"
				if i == 1 {
					denominator = " of 2)"
				}
				if strings.Count(out, denominator) != 7 {
					t.Fatalf("stale receipt denominator:\n%s", out)
				}
				if calls != i+1 {
					t.Fatalf("read %d: resolver called %d times, want %d including all receipts", i+1, calls, i+1)
				}
				names = []string{"second", "peer"}
			}
		})
	}
}

func TestFilterPostsForReaderPreservesAddressingAndRefreshes(t *testing.T) {
	boardInTempHome(t)
	oldSelect := FleetSelect
	t.Cleanup(func() { FleetSelect = oldSelect })
	calls := map[Audience]int{}
	names := []string{"reader"}
	FleetSelect = func(a Audience) ([]string, error) {
		calls[a]++
		if a.Role == "conductor" {
			return names, nil
		}
		return []string{"outsider"}, nil
	}
	aud := &Audience{Role: "conductor"}
	posts := []Post{
		{Seq: 1, To: "reader", Body: "direct"},
		{Seq: 2, Body: "broadcast"},
		{Seq: 3, To: "outsider", Body: "another reader"},
		{Seq: 4, Audience: aud, Body: "notice"},
		{Seq: 5, Audience: aud, Body: "second notice"},
		{Seq: 6, Audience: aud, Mode: ModeAny, Body: "claimed offer"},
		{Seq: 7, Audience: &Audience{Band: 4}, Body: "another audience"},
	}
	if _, granted := ClaimPost(6, "peer"); !granted {
		t.Fatal("could not seed claimed offer")
	}
	got := FilterPostsForReader(posts, "reader")
	want := []int64{1, 2, 4, 5}
	if len(got) != len(want) {
		t.Fatalf("selected posts = %+v", got)
	}
	for i, seq := range want {
		if got[i].Seq != seq {
			t.Fatalf("selected post %d = %d, want %d", i, got[i].Seq, seq)
		}
	}
	names = []string{"successor"}
	got = FilterPostsForReader(posts, "reader")
	if len(got) != 2 || got[0].Seq != 1 || got[1].Seq != 2 {
		t.Fatalf("after handoff: %+v", got)
	}
	for _, selector := range []Audience{*aud, {Band: 4}} {
		if calls[selector] != 2 {
			t.Errorf("selector %+v resolved %d times, want once per call", selector, calls[selector])
		}
	}
	for i, p := range posts {
		if p.Seq != int64(i+1) {
			t.Fatalf("input posts mutated: %+v", posts)
		}
		if viewers := Viewers(p.Seq); len(viewers) != 0 {
			t.Fatalf("filter recorded views for %d: %v", p.Seq, viewers)
		}
	}
	if SeenSeq("reader") != 0 {
		t.Fatal("filter consumed the reader cursor")
	}
	if holder := ClaimHolder(6); holder != "peer" {
		t.Fatalf("filter changed claim holder: %q", holder)
	}
}
