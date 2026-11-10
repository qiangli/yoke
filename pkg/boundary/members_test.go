package boundary

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet/cloudboxfake"
)

func TestAllowed(t *testing.T) {
	active := Member{Email: "bob@example.com", Circle: "family", Status: "active", Mutual: true}
	tests := []struct {
		name   string
		row    *Member
		grants []string
		want   bool
	}{
		{"email", &active, []string{"bob@example.com"}, true},
		{"circle", &active, []string{"family"}, true},
		{"wrong circle", &active, []string{"team"}, false},
		{"inactive", &Member{Email: active.Email, Circle: "family", Status: "pending", Mutual: true}, []string{"family"}, false},
		{"one sided", &Member{Email: active.Email, Circle: "family", Status: "active"}, []string{"family"}, false},
		{"unknown", nil, []string{"family"}, false},
		{"removed", nil, []string{"bob@example.com"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rows []Member
			if tt.row != nil {
				rows = []Member{*tt.row}
			}
			if got := Allowed(active.Email, tt.grants, rows); got != tt.want {
				t.Fatalf("Allowed=%t, want %t", got, tt.want)
			}
		})
	}
}

func TestListCacheOffline(t *testing.T) {
	f := cloudboxfake.New(t)
	tok := f.AddOwner("alice@example.com")
	now := time.Now()
	c := &Client{BaseURL: f.URL(), Token: tok, HTTP: &http.Client{Timeout: time.Second}, Now: func() time.Time { return now }, TTL: time.Second}
	rows, stale, err := c.List(context.Background())
	if err != nil || stale || rows == nil || len(rows) != 0 {
		t.Fatalf("initial list: %#v %t %v", rows, stale, err)
	}
	// The server becomes unavailable; a fresh cache read still succeeds.
	f.Master.Close()
	_, stale, err = c.List(context.Background())
	if err != nil || stale {
		t.Fatalf("cached: stale=%t err=%v", stale, err)
	}
	now = now.Add(2 * time.Second)
	rows, stale, err = c.List(context.Background())
	if err != nil || !stale || len(rows) != 0 {
		t.Fatalf("offline: %#v %t %v", rows, stale, err)
	}
	d, err := c.Decide(context.Background(), "bob@example.com", []string{"family"})
	if err != nil || d.Allowed || !d.Offline || d.ListAge != 2*time.Second {
		t.Fatalf("offline authorization: decision=%+v err=%v", d, err)
	}
	now = now.Add(24 * time.Hour)
	if _, err := c.Decide(context.Background(), "bob@example.com", []string{"family"}); err == nil {
		t.Fatal("list older than 24h authorized")
	}
}

func TestAuthorizationFailureDoesNotUseSnapshot(t *testing.T) {
	f := cloudboxfake.New(t)
	c := &Client{BaseURL: f.URL(), Token: f.AddOwner("alice@example.com")}
	if _, _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.Token = "invalid"
	c.Invalidate()
	if _, stale, err := c.List(context.Background()); err == nil || stale {
		t.Fatalf("auth failure: stale=%t err=%v", stale, err)
	}
}
