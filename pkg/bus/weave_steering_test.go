package bus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestWeaveInstanceSteeringUsesOwningSocket(t *testing.T) {
	isolate(t)
	s := instStore(t)
	inst := openEsme(t, s, "Lucia")
	socket := filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(socket, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// The ownership card has no transport. The separate weave presence card
	// owns the transport and must be matched by the same immutable principal.
	for _, c := range []room.Card{
		{ID: inst.ClaimID(), PID: os.Getpid(), Principal: inst.ClaimID()},
		{ID: "weave-worker", PID: os.Getpid(), Principal: inst.ClaimID(), Binding: "claude:opus5", CtlSock: socket},
		{ID: "foreign-worker", PID: os.Getpid(), Principal: "another-instance", Binding: "claude:opus5", CtlSock: "foreign-socket"},
	} {
		if err := room.Join(c); err != nil {
			t.Fatal(err)
		}
	}
	res, err := Send(SendRequest{From: "manager", To: "esme", Topic: "mb", Body: "steering marker 2370"})
	if err != nil {
		t.Fatal(err)
	}
	received, err := os.ReadFile(socket)
	if err != nil || !strings.Contains(string(received), "steering marker 2370") {
		t.Fatalf("actual control receiver: %q %v; result=%+v", received, err, res)
	}
	if len(res.Deliveries) != 1 || res.Deliveries[0].State != StateDelivered || res.Deliveries[0].Reason != "" {
		t.Fatalf("receipt: %+v", res)
	}
}
