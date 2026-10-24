package rods

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	ygit "github.com/qiangli/yoke/git"
)

func TestOverviewListsEveryRegisteredNounInTempHome(t *testing.T) {
	home := t.TempDir()
	for key, val := range map[string]string{
		"HOME":             home,
		"BASHY_HOME":       filepath.Join(home, ".bashy"),
		"BASHY_FLEET_DIR":  filepath.Join(home, "fleet"),
		"BASHY_BIN_CACHE":  filepath.Join(home, "cache"),
		"BASHY_COORD_DIR":  filepath.Join(home, "coord"),
		"BASHY_SPRINT_DIR": filepath.Join(home, "sprint"),
		"BASHY_ROOM_DIR":   filepath.Join(home, "room"),
	} {
		t.Setenv(key, val)
	}
	repo := filepath.Join(home, "repo")
	if _, err := ygit.Init(ygit.InitOptions{Path: repo}); err != nil {
		t.Fatal(err)
	}
	old, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	rows := Registry().Overview(context.Background())
	want := []string{"agent", "claim", "host", "satellite", "self", "sprint", "src", "weave"}
	if len(rows) != len(want) {
		t.Fatalf("overview rows = %+v", rows)
	}
	for i, row := range rows {
		if row.Noun != want[i] || row.Command != "bashy "+want[i]+" prune --apply" {
			t.Fatalf("row %d = %+v", i, row)
		}
		if want[i] == "host" || want[i] == "satellite" {
			if row.Error == "" {
				t.Fatalf("row %d should report missing owner token", i)
			}
		} else if row.Error != "" {
			t.Fatalf("row %d = %+v", i, row)
		}
	}
	if _, err := os.Stat(filepath.Join(home, "room")); !os.IsNotExist(err) {
		t.Fatal("dry-run created room state")
	}
}
