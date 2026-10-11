package weave

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ask"
)

type approvalReceiptSession struct {
	*fakeSessionClient
	status string
}

func (f *approvalReceiptSession) GetEvents(ctx context.Context, taskID, since string, limit int) (EventsResponse, error) {
	if limit == 0 {
		return f.fakeSessionClient.GetEvents(ctx, taskID, since, limit)
	}
	if f.status == "" {
		return EventsResponse{}, nil
	}
	for i := len(f.appends) - 1; i >= 0; i-- {
		if f.appends[i].Kind != ApprovalDirectiveKind {
			continue
		}
		var d ask.RemoteDirective
		if json.Unmarshal(f.appends[i].Detail, &d) != nil {
			break
		}
		rc, _ := json.Marshal(ask.RemoteReceipt{Directive: d.ID, Approval: d.Approval, Host: d.Host, Status: f.status})
		return EventsResponse{Events: []Event{{ID: "receipt-event", Kind: ApprovalReceiptKind, Detail: rc}}}, nil
	}
	return EventsResponse{}, nil
}

func TestSessionApproveCommandShowsPendingAndRejectedReceipt(t *testing.T) {
	for _, tc := range []struct {
		name, status, want string
	}{
		{name: "pending", want: "pending"},
		{name: "forged account rejected", status: ask.RemoteRejected, want: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &approvalReceiptSession{fakeSessionClient: &fakeSessionClient{}, status: tc.status}
			repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake.fakeSessionClient)
			newSessionClient = func(string, string) SessionClient { return fake }
			old, err := os.Getwd()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Chdir(repo); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chdir(old) })
			out, code := runSprintSession(t, "approve", "0123456789abcdef0123456789abcdef", "--host", "hostA", "--token", "plan-hash", "--verdict", "allow")
			if code != 0 || !strings.Contains(out, tc.want) {
				t.Fatalf("approve output=%q code=%d, want %q", out, code, tc.want)
			}
			if len(fake.appends) != 1 || fake.appends[0].Kind != ApprovalDirectiveKind {
				t.Fatalf("directive append = %+v", fake.appends)
			}
		})
	}
}

func TestSessionApproveCommandRequiresFullApprovalID(t *testing.T) {
	out, code := runSprintSession(t, "approve", "abc", "--host", "hostA", "--token", "plan-hash", "--verdict", "allow")
	if code == 0 || !strings.Contains(out, "full 32-character id") {
		t.Fatalf("approve output=%q code=%d", out, code)
	}
}
