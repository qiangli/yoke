package capability

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
)

func TestLadderSeasonEndFirstSeasonAndIdempotence(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_LADDER_SEASON", "1")
	appendSeasonTestEvents(t, ladder.Event{ID: "a", At: time.Unix(1, 0), Season: 1, Kind: ladder.EventKindCert, Agent: "agent-a", Cert: ladder.Certificate{Kind: ladder.CertL1, ModelVersion: "v1", Season: 1}})
	cmd := NewLeaderboardCmd()
	cmd.SetArgs([]string{"season-end", "--season", "1"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(os.Getenv("BASHY_HOME"), "ladder", "seasons", "1.json")
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("season state missing: %v", err)
	}
	if !strings.Contains(out.String(), "agent-a") {
		t.Fatalf("output lacks agent: %s", out.String())
	}
	cmd = NewLeaderboardCmd()
	cmd.SetArgs([]string{"season-end", "--season", "1"})
	cmd.SetOut(&bytes.Buffer{})
	if err := cmd.Execute(); err == nil {
		t.Fatal("second run should refuse overwrite")
	}
}

func TestLadderSeasonEndDryRunAndAnchorFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_LADDER_SEASON", "1")
	if err := os.MkdirAll(filepath.Join(home, "ladder"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "ladder", "anchors.txt"), []byte("agent-a,agent-b\n"), 0600); err != nil {
		t.Fatal(err)
	}
	events := []ladder.Event{
		{ID: "a", At: time.Unix(1, 0), Season: 1, Kind: ladder.EventKindManage, Agent: "agent-a", Score: 1, Opponent: ladder.Rating{R: 1800, RD: 20}},
		{ID: "b", At: time.Unix(2, 0), Season: 1, Kind: ladder.EventKindManage, Agent: "agent-b", Score: 1, Opponent: ladder.Rating{R: 1700, RD: 20}},
	}
	for i := 0; i < 8; i++ {
		for _, a := range []string{"agent-a", "agent-b"} {
			events = append(events,
				ladder.Event{ID: a + "m" + string(rune('a'+i)), At: time.Unix(int64(3+i*2), 0), Season: 1, Kind: ladder.EventKindManage, Agent: a, Score: 1, Opponent: ladder.Rating{R: 1500, RD: 50}},
				ladder.Event{ID: a + "c" + string(rune('a'+i)), At: time.Unix(int64(4+i*2), 0), Season: 1, Kind: ladder.EventKindDelivery, Agent: a, Story: a + string(rune('a'+i)), Points: 1, Outcome: 1})
		}
	}
	appendSeasonTestEvents(t, events...)
	cmd := NewLeaderboardCmd()
	cmd.SetArgs([]string{"season-end", "--season", "1", "--dry-run", "--json"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Lines ladder.Lines `json:"lines"`
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Lines.L4Manage <= 0 {
		t.Fatalf("anchor L4 manage line not set: %+v", got.Lines)
	}
	if _, err := os.Stat(filepath.Join(home, "ladder", "seasons", "1.json")); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote season file: %v", err)
	}
}

func appendSeasonTestEvents(t *testing.T, events ...ladder.Event) {
	t.Helper()
	s, err := ladder.OpenStore(ladder.DefaultStorePath())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
}
