package bus

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// Story 188896bb: `mb send conductor:<sprint>` stores the seat topic in To,
// stamps Post.Seat = "conductor:<sprint>", steers and checks read state against
// the live seat holder (instance UUID or agent name), and stamps Post.FromSeats
// when the conductor replies so workers can identify its sprint seat from post
// metadata even when four instances share the conductor's family name.
func TestSendConductorSeatReachesHolderAndStampsSeatMetadata(t *testing.T) {
	isolate(t)
	s := instStore(t)
	t.Setenv(fleet.InstanceCapEnv, "6")

	// Four active Esme..Esme-4 instances share family "claude-opus5.5",
	// reproducing the exact live host state.
	opusFam := fleet.Family{Name: "claude-opus5.5", Display: "Esme", Policy: "single", Bindings: []string{"claude:opus5.5"}}
	openOpus := func(label string) fleet.Instance {
		inst, oerr := s.Open(opusFam, fleet.OpenOptions{Label: label})
		if oerr != nil {
			t.Fatalf("open %s: %v", label, oerr)
		}
		return inst
	}
	conductorInst := openOpus("Esme")
	_ = openOpus("Esme-2")
	_ = openOpus("Esme-3")
	_ = openOpus("Esme-4")

	// Register live control socket on conductorInst's room card to observe steering.
	var steeredSock, steeredText string
	prevSteer := SteerFrame
	SteerFrame = func(sock, text string) error {
		steeredSock, steeredText = sock, text
		return nil
	}
	t.Cleanup(func() { SteerFrame = prevSteer })

	if err := room.Join(room.Card{
		ID:        "instance:" + conductorInst.UUID,
		Principal: "dhnt:agent/" + conductorInst.UUID,
		Nick:      "Esme",
		Tool:      "claude",
		Model:     "opus5",
		Mode:      "interactive",
		PID:       os.Getpid(),
		OwnerPID:  os.Getpid(),
		CtlSock:   "/tmp/fake-conductor-678.sock",
	}); err != nil {
		t.Fatal(err)
	}

	prevAuth := RoleReaderAuthorizer
	RoleReaderAuthorizer = AuthorizeByHolder
	t.Cleanup(func() { RoleReaderAuthorizer = prevAuth })

	withHostRoles(t,
		HostRole{
			Label:          "conductor:678",
			Topic:          "conductor.678",
			Holder:         "claude-opus5.5",
			HolderInstance: conductorInst.UUID,
		},
		HostRole{
			Label:  "conductor:576",
			Topic:  "conductor.576",
			Holder: "claude-opus5.5",
		},
	)

	// 1. Bare `mb send claude-opus5.5` fails as ambiguous AND lists the held conductor seats.
	_, ambErr := Send(SendRequest{From: "muse-wonder-full", To: "claude-opus5.5", Topic: "mb", Body: "DONE fa9fcc8d"})
	if ambErr == nil || !Refusal(ambErr) {
		t.Fatalf("expected ambiguous refusal for bare claude-opus5.5, got %v", ambErr)
	}
	for _, want := range []string{"conductor:678", "conductor:576", "bashy mb send conductor:"} {
		if !strings.Contains(ambErr.Error(), want) {
			t.Fatalf("ambiguous error missing %q:\n%s", want, ambErr.Error())
		}
	}

	// 2. `mb send conductor:678` succeeds, stamps Seat="conductor:678", steers
	//    conductorInst's live socket, and is Directed to conductorInst and claude-opus5.5.
	res, err := Send(SendRequest{From: "muse-wonder-full", To: "conductor:678", Topic: "mb", Body: "DONE fa9fcc8de90d"})
	if err != nil {
		t.Fatalf("Send(conductor:678): %v", err)
	}
	if len(res.Deliveries) != 1 || !res.Deliveries[0].Steered || res.Deliveries[0].To != "conductor:678" {
		t.Fatalf("deliveries = %+v, want steered to conductor:678", res.Deliveries)
	}
	if steeredSock != "/tmp/fake-conductor-678.sock" || !strings.Contains(steeredText, "DONE fa9fcc8de90d") {
		t.Fatalf("steered = (%q, %q), want conductor socket + body", steeredSock, steeredText)
	}

	workerPost := lastPost(t)
	if workerPost.To != "conductor.678" || workerPost.Seat != "conductor:678" || workerPost.ToParty != nil {
		t.Fatalf("worker post = %+v, want To=conductor.678 Seat=conductor:678 ToParty=nil", workerPost)
	}
	if !workerPost.Directed(conductorInst.UUID) {
		t.Fatal("worker post to conductor:678 was not Directed to holder instance UUID")
	}

	// 3. Conductor replies; Post.FromSeats records ["conductor:576", "conductor:678"]
	//    and `mb show` displays seat metadata.
	prevNames, prevResolve := FleetNames, FleetResolveName
	FleetNames = func() []string { return []string{"muse-wonder-full"} }
	FleetResolveName = func(n string) string {
		if strings.EqualFold(n, "muse-wonder-full") {
			return "muse-wonder-full"
		}
		return ""
	}
	t.Cleanup(func() { FleetNames, FleetResolveName = prevNames, prevResolve })
	if _, err := Send(SendRequest{From: "claude-opus5.5", To: "muse-wonder-full", Topic: "mb", Body: "DESIGN approved"}); err != nil {
		t.Fatalf("conductor reply: %v", err)
	}
	replyPost := lastPost(t)
	if len(replyPost.FromSeats) != 2 || replyPost.FromSeats[0] != "conductor:576" || replyPost.FromSeats[1] != "conductor:678" {
		t.Fatalf("conductor reply FromSeats = %v, want [conductor:576 conductor:678]", replyPost.FromSeats)
	}

	cmd := NewMessageBoardCmd()
	var showOut bytes.Buffer
	cmd.SetOut(&showOut)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"show", "1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("mb show 1: %v", err)
	}
	if !strings.Contains(showOut.String(), "seat   conductor:678") {
		t.Fatalf("mb show 1 missing seat line:\n%s", showOut.String())
	}
}

// Story 188896bb (conductor identity condition):
//   - An unclaimed seat is auto-taken ONLY when the ancestor session card
//     matches BOTH tool AND model (or the exact registered agent name).
//   - A `bashy muse` session running a DIFFERENT muse model (`muse-spark1.3`)
//     is refused when trying `--as muse-wonder-full`.
//   - A `bashy muse` session card with NO recorded model (`Model == ""` and
//     `Binding == ""`) is refused with guidance (`BASHY_AGENT=` /
//     `bashy instance open`) instead of guessing.
func TestResolveAuthoredActorToolAndModelIdentityCondition(t *testing.T) {
	isolateAuthoredActor(t)

	FleetNames = func() []string { return []string{"muse-wonder-full", "muse-spark1.3"} }
	FleetResolveName = func(name string) string {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "muse-wonder-full":
			return "muse-wonder-full"
		case "muse-spark1.3":
			return "muse-spark1.3"
		default:
			return ""
		}
	}
	prevLookup := FleetAgentLookup
	FleetAgentLookup = func(name string) (fleet.Agent, bool) {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "muse-wonder-full":
			return fleet.Agent{Name: "muse-wonder-full", Tool: "muse", Model: "muse-wonder-full"}, true
		case "muse-spark1.3":
			return fleet.Agent{Name: "muse-spark1.3", Tool: "muse", Model: "muse-spark1.3"}, true
		default:
			return fleet.Agent{}, false
		}
	}
	t.Cleanup(func() { FleetAgentLookup = prevLookup })

	DetectHarness = func() (string, bool) { return "muse", true }
	for _, env := range []string{"BASHY_AGENT", "WEAVE_AGENT", "BASHY_AGENT_ID", "WEAVE_CONDUCTOR"} {
		t.Setenv(env, "")
	}

	// Case 1: Session card has tool="muse" but NO model -> MUST refuse with guidance, never guess.
	if err := room.Join(room.Card{
		ID: "muse", Nick: "muse", Tool: "muse", Mode: "interactive",
		PID: os.Getpid(), OwnerPID: os.Getpid(), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveAuthoredActor("muse-wonder-full")
	if err == nil {
		t.Fatal("unknown-model tool session guessed and took muse-wonder-full")
	}
	for _, want := range []string{"no matching live session claim", "no recorded model", "BASHY_AGENT=muse-wonder-full", "bashy instance open muse-wonder-full"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("unknown-model refusal missing %q:\n%v", want, err)
		}
	}
	if _, live, _ := room.Find(room.AgentClaimID("muse-wonder-full")); live {
		t.Fatal("refused unknown-model session took seat")
	}

	// Case 2: Session card has tool="muse" + model="muse-spark1.3" -> MUST refuse `--as muse-wonder-full`.
	if err := room.Join(room.Card{
		ID: "muse", Nick: "muse", Tool: "muse", Model: "muse-spark1.3", Binding: "muse:muse-spark1.3",
		Mode: "interactive", PID: os.Getpid(), OwnerPID: os.Getpid(), Principal: "operator",
	}); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveAuthoredActor("muse-wonder-full")
	if err == nil {
		t.Fatal("mismatched-model muse session was allowed to post as muse-wonder-full")
	}
	for _, want := range []string{"no matching live session claim", "muse-spark1.3", "muse-wonder-full", "bashy instance open"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("mismatched-model refusal missing %q:\n%v", want, err)
		}
	}
	if _, live, _ := room.Find(room.AgentClaimID("muse-wonder-full")); live {
		t.Fatal("refused mismatched-model session took muse-wonder-full seat")
	}

	// Case 3: Same `muse:muse-spark1.3` session calling `--as muse-spark1.3` (matching tool AND model) -> ACCEPTED.
	got, err := ResolveAuthoredActor("muse-spark1.3")
	if err != nil || got != "muse-spark1.3" {
		t.Fatalf("matching tool+model ResolveAuthoredActor(muse-spark1.3) = %q, %v; want muse-spark1.3", got, err)
	}
	if _, live, _ := room.Find(room.AgentClaimID("muse-spark1.3")); !live {
		t.Fatal("matching tool+model session did not claim muse-spark1.3 seat")
	}
}
