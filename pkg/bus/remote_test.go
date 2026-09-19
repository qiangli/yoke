// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package bus

import (
	"errors"
	"strings"
	"testing"
)

// wireRemote installs a roster of two hosts and a recording relay for one test.
func wireRemote(t *testing.T, relayErr error) (*[]RemoteMessage, *int) {
	t.Helper()
	prevR, prevS, prevF := RemoteResolve, RemoteSend, RemoteSender
	t.Cleanup(func() { RemoteResolve, RemoteSend, RemoteSender = prevR, prevS, prevF })
	roster := []string{"plinth@noviwin1", "rafter@dragon", "twin@dragon", "twin@noviwin1"}
	resolves := 0
	RemoteResolve = func(target string) (RemoteRoute, error) {
		resolves++
		t2 := strings.TrimSpace(target)
		var hits []string
		for _, p := range roster {
			if strings.EqualFold(p, t2) || strings.EqualFold(strings.SplitN(p, "@", 2)[0], t2) {
				hits = append(hits, p)
			}
		}
		switch len(hits) {
		case 0:
			return RemoteRoute{}, ErrRemoteUnknown
		case 1:
			return RemoteRoute{Participant: hits[0], Host: strings.SplitN(hits[0], "@", 2)[1], Session: "task-1"}, nil
		}
		return RemoteRoute{}, ErrRemoteAmbiguous
	}
	sent := &[]RemoteMessage{}
	RemoteSend = func(m RemoteMessage) (RemoteReceipt, error) {
		if relayErr != nil {
			return RemoteReceipt{}, relayErr
		}
		*sent = append(*sent, m)
		return RemoteReceipt{EventID: "ev-" + m.ID}, nil
	}
	RemoteSender = func() (string, string) { return "tester@dragon", "dhnt:agent/tester@me@example.com" }
	return sent, &resolves
}

func TestIsRemoteAddress(t *testing.T) {
	for in, want := range map[string]bool{
		"plinth@noviwin1": true, "plinth": false, "@host": false, "name@": false,
		"a b@host": false, "name@ho st": false, "alice@example.com": true, // the roster decides
	} {
		if got := IsRemoteAddress(in); got != want {
			t.Errorf("IsRemoteAddress(%q) = %v, want %v", in, got, want)
		}
	}
}

// The email model: relay first, then the outbox copy with the SAME id, and a
// receipt that says queued — never delivered.
func TestSendRemoteRelaysThenKeepsAnOutboxCopy(t *testing.T) {
	isolate(t)
	sent, _ := wireRemote(t, nil)

	before := boardLen(t)
	res, err := Send(SendRequest{From: "tester", To: "plinth@noviwin1", Topic: "sprint-217", Body: "rebase first"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if res.Kind != SendRemote || res.ID == "" || res.Label != "plinth@noviwin1" {
		t.Fatalf("result = %+v", res)
	}
	if len(*sent) != 1 || (*sent)[0].ID != res.ID || (*sent)[0].To != "plinth@noviwin1" || (*sent)[0].From != "tester@dragon" || (*sent)[0].Session != "task-1" {
		t.Fatalf("relay envelope = %+v (want id %s)", *sent, res.ID)
	}
	if after := boardLen(t); after != before+1 {
		t.Fatalf("board grew %d → %d, want exactly one outbox copy", before, after)
	}
	posts, _ := Posts()
	last := posts[len(posts)-1]
	if last.ID != res.ID || last.To != "plinth@noviwin1" || last.Seq != res.Seq {
		t.Fatalf("outbox copy = %+v, want id %s to plinth@noviwin1 seq %d", last, res.ID, res.Seq)
	}
	if len(res.Deliveries) != 1 || res.Deliveries[0].State != StateQueued {
		t.Fatalf("deliveries = %+v, want one queued (never delivered)", res.Deliveries)
	}
}

// A relay refusal posts NOTHING locally — a sent copy nobody will receive is
// the receipt this package forbids.
func TestSendRemoteRelayRefusalPostsNothing(t *testing.T) {
	isolate(t)
	wireRemote(t, errors.New("503 relay down"))
	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", To: "plinth@noviwin1", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "nothing was posted") {
		t.Fatalf("want a refusal saying nothing was posted, got %v", err)
	}
	if after := boardLen(t); after != before {
		t.Fatalf("board grew %d → %d on a refused relay", before, after)
	}
}

// A bare name the local ladder cannot place may still be ONE colleague on the
// session; two colleagues with that name is an ambiguity, not a guess.
func TestSendRemoteBareNameFallsThroughAndAmbiguityStops(t *testing.T) {
	isolate(t)
	sent, _ := wireRemote(t, nil)
	res, err := Send(SendRequest{From: "tester", To: "rafter", Body: "hi"})
	if err != nil || res.Kind != SendRemote || res.Label != "rafter@dragon" || len(*sent) != 1 {
		t.Fatalf("bare unique name: res=%+v err=%v sent=%d", res, err, len(*sent))
	}
	before := boardLen(t)
	_, err = Send(SendRequest{From: "tester", To: "twin", Body: "hi"})
	if !errors.Is(err, ErrRemoteAmbiguous) {
		t.Fatalf("want ErrRemoteAmbiguous for a name on two hosts, got %v", err)
	}
	if boardLen(t) != before || len(*sent) != 1 {
		t.Fatal("an ambiguous send must post nothing and relay nothing")
	}
}

// With no relay wired, behaviour is exactly what it was: an unknown target is
// reported with choices and nothing is posted.
func TestSendWithoutRelayIsUnchanged(t *testing.T) {
	isolate(t)
	prevR, prevS := RemoteResolve, RemoteSend
	RemoteResolve, RemoteSend = nil, nil
	t.Cleanup(func() { RemoteResolve, RemoteSend = prevR, prevS })
	before := boardLen(t)
	_, err := Send(SendRequest{From: "tester", To: "plinth@noviwin1", Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "nothing was posted") {
		t.Fatalf("want the local unresolved error, got %v", err)
	}
	if boardLen(t) != before {
		t.Fatal("board grew on an unresolved target")
	}
}

// Every post now carries a universal id, minted on write.
func TestPostsCarryAUUID(t *testing.T) {
	isolate(t)
	seq, err := PostMessageSeq(Post{From: "tester", Body: "local"})
	if err != nil {
		t.Fatal(err)
	}
	posts, _ := Posts()
	for _, p := range posts {
		if p.Seq == seq {
			if len(p.ID) != 36 || strings.Count(p.ID, "-") != 4 {
				t.Fatalf("post %d id = %q, want a dashed uuid", seq, p.ID)
			}
			return
		}
	}
	t.Fatal("post not found")
}
