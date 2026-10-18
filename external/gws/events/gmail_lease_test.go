package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/schedule"

	"github.com/qiangli/yoke/external/gws"
)

// fakeRun is a recorded-response Runner: each call pops the next response.
type fakeRun struct {
	resp  []fakeResp
	argvs [][]string
}

type fakeResp struct {
	out string
	err error
}

func (f *fakeRun) run(ctx context.Context, argv []string) ([]byte, error) {
	f.argvs = append(f.argvs, argv)
	if len(f.resp) == 0 {
		return nil, errors.New("unexpected call")
	}
	r := f.resp[0]
	f.resp = f.resp[1:]
	return []byte(r.out), r.err
}

var exit1 = errors.New("exit status 1")

func TestGmailHistoryPaginationIsNotTheCursor(t *testing.T) {
	f := &fakeRun{resp: []fakeResp{
		{out: `{"history":[{"id":"101","messagesAdded":[]},{"id":"102"}],"nextPageToken":"PAGE2","historyId":"105"}`},
		{out: `{"history":[{"id":"103"}],"historyId":"105"}`},
	}}
	b, err := GmailHistorySource{Run: f.run, User: "me"}.Poll(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if b.Next != "105" || len(b.Events) != 3 || b.Events[2].ID != "103" {
		t.Fatalf("batch = %+v", b)
	}
	if !strings.Contains(strings.Join(f.argvs[1], " "), `"pageToken":"PAGE2"`) || !strings.Contains(strings.Join(f.argvs[1], " "), `"startHistoryId":"100"`) {
		t.Fatalf("page 2 must keep the cursor and add the page token: %v", f.argvs[1])
	}
	if f.argvs[0][0] != "gws" || f.argvs[0][1] != "gmail" {
		t.Fatalf("must invoke the registered gws command: %v", f.argvs[0])
	}
	if _, err := (GmailHistorySource{Run: f.run}).Poll(context.Background(), "1"); codeOf(err) != gws.CodeInvalidInput {
		t.Fatal("user must be explicit")
	}
}

func TestGmailHistoryBoundIsResumable(t *testing.T) {
	var resp []fakeResp
	for i := 0; i < MaxHistoryPages; i++ {
		resp = append(resp, fakeResp{out: `{"history":[{"id":"` + string(rune('a'+i)) + `"}],"nextPageToken":"more","historyId":"999"}`})
	}
	b, err := GmailHistorySource{Run: (&fakeRun{resp: resp}).run, User: "me"}.Poll(context.Background(), "100")
	if err != nil {
		t.Fatal(err)
	}
	if b.Next != "j" || b.Next == "999" {
		t.Fatalf("a bounded walk must resume from the last record, not the head: %q", b.Next)
	}
}

func TestGmailErrorClassificationAndRedaction(t *testing.T) {
	cases := []struct {
		body string
		code gws.Code
	}{
		{`{"error":{"code":404,"message":"Requested entity was not found.","status":"NOT_FOUND"}}`, gws.CodeCursorExpired},
		{`{"error":{"code":401,"message":"token ` + canary + ` invalid","status":"UNAUTHENTICATED"}}`, gws.CodeExpiredCredentials},
		{`{"error":{"code":403,"message":"denied","status":"PERMISSION_DENIED"}}`, gws.CodePermissionDenied},
		{`{"error":{"code":403,"message":"slow","errors":[{"reason":"userRateLimitExceeded"}]}}`, gws.CodeRateLimit},
		{`{"error":{"code":429,"message":"slow","status":"RESOURCE_EXHAUSTED"}}`, gws.CodeRateLimit},
		{`{"error":{"code":503,"message":"backend"}}`, gws.CodeProviderFailure},
		{`not json; Authorization: Bearer ` + canary, gws.CodeProviderFailure},
	}
	for _, c := range cases {
		err := classify([]byte(c.body), errors.New("exit status 1 "+canary), "gmail history")
		if codeOf(err) != c.code {
			t.Errorf("%s → %v, want %s", c.body, err, c.code)
		}
	}
	// Through the poller, a provider error body carrying the canary is masked.
	s := openStore(t, t.TempDir())
	defer s.Close()
	_ = s.Resync("100", "initial")
	f := &fakeRun{resp: []fakeResp{{out: `{"error":{"code":401,"message":"bad token ` + canary + `"}}`, err: exit1}}}
	p := &Poller{Source: GmailHistorySource{Run: f.run, User: "me"}, Store: s, Sleep: noSleep, Secrets: []string{canary},
		Handler: func(context.Context, Event) error { return nil }}
	_, err := p.RunOnce(context.Background())
	if codeOf(err) != gws.CodeExpiredCredentials || strings.Contains(err.Error(), canary) || !strings.Contains(err.Error(), gws.Redacted) {
		t.Fatalf("redaction failed: %v", err)
	}
	// Subprocess failure text (no JSON body) is masked as well.
	_ = s.Resync("100", "retry")
	f.resp = []fakeResp{{out: "", err: errors.New("gws: panic with " + canary)}}
	p.Backoff = Backoff{Attempts: 1}
	_, err = p.RunOnce(context.Background())
	if err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("subprocess failure leaked the canary: %v", err)
	}
	// The handler's own error text is masked too.
	_ = s.Resync("100", "retry2")
	f.resp = []fakeResp{{out: `{"history":[{"id":"101"}],"historyId":"101"}`}}
	p.Handler = func(context.Context, Event) error { return errors.New("webhook 500 for " + canary) }
	if _, err = p.RunOnce(context.Background()); err == nil || strings.Contains(err.Error(), canary) {
		t.Fatalf("handler failure leaked the canary: %v", err)
	}
}

func TestGmailWatchLeaseRenewal(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	s := openStore(t, t.TempDir())
	defer s.Close()
	_ = s.PutLease(Lease{Kind: LeaseGmailWatch, Name: "me", Expires: now.Add(6 * time.Hour)})
	_ = s.PutLease(Lease{Kind: LeaseEventsSubscription, Name: "subscriptions/far", Expires: now.Add(72 * time.Hour)})
	newExp := now.Add(GmailWatchMaxLifetime)
	f := &fakeRun{resp: []fakeResp{
		{out: `{"error":{"code":503,"message":"try again"}}`, err: exit1},
		{out: `{"historyId":"777","expiration":"` + itoa(newExp.UnixMilli()) + `"}`},
	}}
	r := Renewer{Renew: CommandRenewer(f.run, "me", "projects/p/topics/t"), Now: func() time.Time { return now }, Sleep: noSleep}
	renewed, err := r.RenewDue(context.Background(), s)
	if err != nil || len(renewed) != 1 || !renewed[0].Expires.Equal(newExp) {
		t.Fatalf("renewed = %+v, %v", renewed, err)
	}
	if len(f.argvs) != 2 || strings.Join(f.argvs[1][:4], " ") != "gws gmail users watch" || !strings.Contains(strings.Join(f.argvs[1], " "), "projects/p/topics/t") {
		t.Fatalf("watch renewal argv = %v", f.argvs)
	}
	for _, l := range s.Checkpoint().Leases {
		if l.Kind == LeaseGmailWatch && !l.Expires.Equal(newExp) {
			t.Fatal("renewed expiry not persisted")
		}
		if l.Kind == LeaseEventsSubscription && !l.Expires.Equal(now.Add(72*time.Hour)) {
			t.Fatal("a lease outside the margin must not be renewed")
		}
	}
	hid, _, err := ParseGmailWatch([]byte(`{"historyId":"777","expiration":"1"}`))
	if err != nil || hid != "777" {
		t.Fatal("watch response historyId is the resync cursor")
	}
	if _, _, err := ParseGmailWatch([]byte(`{}`)); codeOf(err) != gws.CodeProviderFailure {
		t.Fatal("incomplete watch response must not be assumed renewed")
	}
}

func TestEventsSubscriptionLeaseRenewal(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	s := openStore(t, t.TempDir())
	defer s.Close()
	old := now.Add(time.Hour)
	_ = s.PutLease(Lease{Kind: LeaseEventsSubscription, Name: "subscriptions/abc", Expires: old})
	f := &fakeRun{resp: []fakeResp{
		{out: `{"name":"operations/1","done":false}`},
	}}
	r := Renewer{Renew: CommandRenewer(f.run, "me", "t"), Now: func() time.Time { return now }, Sleep: noSleep, Backoff: Backoff{Attempts: 1}}
	if _, err := r.RenewDue(context.Background(), s); codeOf(err) != gws.CodePartialMutation {
		t.Fatalf("unconfirmed renewal must be unknown-outcome, got %v", err)
	}
	if !s.Checkpoint().Leases[0].Expires.Equal(old) {
		t.Fatal("failed renewal must leave the stored expiry unchanged")
	}
	if got := strings.Join(f.argvs[0], " "); !strings.Contains(got, "gws events subscriptions patch") || !strings.Contains(got, `"updateMask":"ttl"`) {
		t.Fatalf("events renew argv = %s", got)
	}
	exp := now.Add(4 * time.Hour)
	f.resp = []fakeResp{{out: `{"done":true,"response":{"name":"subscriptions/abc","expireTime":"` + exp.Format(time.RFC3339) + `"}}`}}
	renewed, err := r.RenewDue(context.Background(), s)
	if err != nil || len(renewed) != 1 || !s.Checkpoint().Leases[0].Expires.Equal(exp) {
		t.Fatalf("renew = %+v %v", renewed, err)
	}
	// A lease that already lapsed is never silently renewed.
	r.Now = func() time.Time { return exp.Add(time.Second) }
	if _, err := r.RenewDue(context.Background(), s); codeOf(err) != gws.CodeCursorExpired {
		t.Fatalf("lapsed lease: %v", err)
	}
	// A renewal that does not extend the lease is a failure, not success.
	r.Now = func() time.Time { return exp.Add(-time.Minute) }
	f.resp = []fakeResp{{out: `{"done":true,"response":{"expireTime":"` + exp.Format(time.RFC3339) + `"}}`}}
	if _, err := r.RenewDue(context.Background(), s); codeOf(err) != gws.CodeProviderFailure {
		t.Fatalf("non-extending renewal: %v", err)
	}
	if _, err := CommandRenewer(f.run, "", "")(context.Background(), Lease{Kind: "drive-channel"}); codeOf(err) != gws.CodeUnsupported {
		t.Fatal("unknown lease kinds are unsupported")
	}
}

func TestRenewalJobUsesExistingScheduler(t *testing.T) {
	argv := GmailWatchArgv("me", "projects/p/topics/t")
	j, err := RenewalJob("gmail-watch-me", GmailWatchRenewEvery, GmailWatchMaxLifetime, argv)
	if err != nil {
		t.Fatal(err)
	}
	next, err := schedule.ComputeNext(j, j.CreatedAt)
	if err != nil || next.Sub(j.CreatedAt) != GmailWatchRenewEvery {
		t.Fatalf("next = %v %v", next, err)
	}
	if j.Command[0] != "gws" || j.Kind != "every" {
		t.Fatalf("job = %+v", j)
	}
	if _, err := RenewalJob("x", 7*24*time.Hour, GmailWatchMaxLifetime, argv); codeOf(err) != gws.CodeInvalidInput {
		t.Fatal("a cadence that lets the lease lapse must be refused")
	}
	raw, _ := json.Marshal(j)
	if strings.Contains(string(raw), canary) {
		t.Fatal("job leaked a credential")
	}
}

func TestGwsRunnerRefusesArbitraryCommands(t *testing.T) {
	if _, err := GwsRunner("/bin/sh")(context.Background(), []string{"sh", "-c", "true"}); codeOf(err) != gws.CodeInvalidInput {
		t.Fatal("runner must execute only the registered gws command")
	}
	if _, err := GwsRunner("")(context.Background(), []string{"gws"}); codeOf(err) != gws.CodeInvalidInput {
		t.Fatal("runner needs a resolved binary")
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }
