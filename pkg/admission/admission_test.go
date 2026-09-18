package admission

import (
	"strings"
	"testing"
)

func TestMixedPrioritySelectsUrgentHeaderBeforeEarlierBulk(t *testing.T) {
	items := []Item{
		{Source: "meet", ID: "1", Priority: PriorityInformational, Body: strings.Repeat("n", 500), ArtifactRef: "open meet:1", OverflowRef: "list meet"},
		{Source: "bus", ID: "9", Priority: PriorityUrgent, Topic: "BLOCKED", Body: strings.Repeat("b", 500), ArtifactRef: "open bus:9", OverflowRef: "list bus"},
	}
	got, err := Render(items, Options{BudgetBytes: 800})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "bus:9") || strings.Contains(got.Text, strings.Repeat("n", 20)) {
		t.Fatalf("priority projection:\n%s", got.Text)
	}
	if got.Report.RenderedBytes != len(got.Text) || len(got.Text) > 800 {
		t.Fatalf("rendered=%d len=%d", got.Report.RenderedBytes, len(got.Text))
	}
}

func TestUTF8PrefixStopsAtRuneBoundary(t *testing.T) {
	got, err := UTF8Prefix("ab界cd", 4)
	if err != nil {
		t.Fatal(err)
	}
	if got != "ab" {
		t.Fatalf("prefix = %q", got)
	}
	if _, err := UTF8Prefix(string([]byte{0xff}), 1); err == nil {
		t.Fatal("invalid UTF-8 accepted")
	}
}

func TestOverflowIsVersionedAndUnrepresentedItemsAreNotAcknowledged(t *testing.T) {
	acked := []string{}
	items := []Item{
		{Source: "bus", ID: "1", Priority: PriorityUrgent, Body: "BLOCKED", ArtifactRef: "open 1", OverflowRef: "list bus", Acknowledge: func() error { acked = append(acked, "1"); return nil }},
		{Source: "bus", ID: "2", Priority: PriorityInformational, Body: strings.Repeat("x", 800), OverflowRef: "list bus", Acknowledge: func() error { acked = append(acked, "2"); return nil }},
	}
	got, err := Render(items, Options{BudgetBytes: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, `"schema_version":"`+OverflowSchemaVersion+`"`) {
		t.Fatalf("missing versioned overflow:\n%s", got.Text)
	}
	if err := got.Commit(); err != nil {
		t.Fatal(err)
	}
	if strings.Join(acked, ",") != "1" {
		t.Fatalf("acknowledged %v", acked)
	}
}

func TestLargeBatchIsBoundedDeterministicAndDigestSensitive(t *testing.T) {
	items := make([]Item, 1200)
	for i := range items {
		items[i] = Item{Source: "host", ID: string(rune(0x1000 + i)), Priority: PriorityInformational, Body: strings.Repeat("界", 300), ArtifactRef: "bashy inbox --id item", OverflowRef: "bashy inbox --peek"}
	}
	a, err := Render(items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := Render(items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != b.Text || a.Report.ContentDigest != b.Report.ContentDigest {
		t.Fatal("same input rendered nondeterministically")
	}
	if len(a.Text) > DefaultTurnBytes || a.Report.InputBytes < 1<<20 {
		t.Fatalf("rendered=%d input=%d", len(a.Text), a.Report.InputBytes)
	}
	items[len(items)-1].Body += "x"
	c, err := Render(items, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Report.ContentDigest == a.Report.ContentDigest {
		t.Fatal("digest ignored changed content")
	}
}

func TestRenderFailureAcknowledgesNothing(t *testing.T) {
	acked := false
	_, err := Render([]Item{{Source: "bus", ID: "1", Priority: PriorityUrgent, Body: strings.Repeat("x", 600), Acknowledge: func() error { acked = true; return nil }}}, Options{})
	if err == nil || acked {
		t.Fatalf("err=%v acked=%v", err, acked)
	}
}
