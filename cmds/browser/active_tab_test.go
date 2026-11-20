package browsercmd

import (
	"strings"
	"testing"
)

func TestActiveTabIDPicksTheOneActiveTab(t *testing.T) {
	id, err := activeTabID(`[{"id":11,"active":false,"title":"a","url":"https://a"},{"id":42,"active":true,"title":"b","url":"https://b"}]`)
	if err != nil || id != 42 {
		t.Fatalf("got %d, %v; want 42", id, err)
	}
}

func TestActiveTabIDRefusesNoneOrSeveral(t *testing.T) {
	if _, err := activeTabID(`[{"id":11,"active":false}]`); err == nil || !strings.Contains(err.Error(), "no tab is active") {
		t.Fatalf("none active: %v", err)
	}
	_, err := activeTabID(`[{"id":11,"active":true,"title":"a"},{"id":12,"active":true,"title":"b"}]`)
	if err == nil || !strings.Contains(err.Error(), "--id 11") || !strings.Contains(err.Error(), "--id 12") {
		t.Fatalf("several active must list candidates: %v", err)
	}
}
