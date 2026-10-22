package digitalocean_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

func newTestClient(t *testing.T, fake *fakedo.Server) *digitalocean.Client {
	t.Helper()
	c, err := digitalocean.NewClient(digitalocean.ClientConfig{
		BaseURL:      fake.URL,
		Token:        fakedo.TestToken,
		PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestTrash_SuccessAndIdempotency(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	// 1. Seed cloudbox-owned droplet
	dID := fake.AddDroplet("sat-node-1", "bashy-lease", "bashy-owner-sat")
	fakeDroplet, ok := fake.Droplet(dID)
	if !ok || fakeDroplet.Status != "active" {
		t.Fatalf("expected droplet active, got %+v", fakeDroplet)
	}

	fixedNow := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	grace := 2 * time.Hour
	expectedPurge := fixedNow.Add(grace).Unix()

	// 2. Trash droplet
	res, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{
		DropletID:     dID,
		Grace:         grace,
		GraceExplicit: true,
		Reason:        "stuck at boot",
		Now:           fixedNow,
	})
	if err != nil {
		t.Fatalf("trash droplet: %v", err)
	}
	if res.Status != "trashed" {
		t.Fatalf("expected status 'trashed', got %q", res.Status)
	}
	if res.PurgeAfter != expectedPurge {
		t.Fatalf("expected purge %d, got %d", expectedPurge, res.PurgeAfter)
	}
	if res.Reason != "stuck-at-boot" {
		t.Fatalf("expected reason 'stuck-at-boot', got %q", res.Reason)
	}
	if !res.PoweredOff {
		t.Fatal("expected powered_off true")
	}

	// Verify server state
	updated, ok := fake.Droplet(dID)
	if !ok {
		t.Fatal("droplet not found on fake server")
	}
	if updated.Status != "off" {
		t.Fatalf("expected status 'off', got %q", updated.Status)
	}
	expectedTags := []string{
		"bashy-lease",
		"bashy-owner-sat",
		"bashy-trash",
		"bashy-purge-after-1792677600",
		"bashy-trash-reason-stuck-at-boot",
	}
	for _, exp := range expectedTags {
		found := false
		for _, tag := range updated.Tags {
			if tag == exp {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("tag %q missing from droplet tags: %v", exp, updated.Tags)
		}
	}

	// Verify tags were created on DO
	for _, exp := range []string{"bashy-trash", "bashy-purge-after-1792677600", "bashy-trash-reason-stuck-at-boot"} {
		if !fake.HasTag(exp) {
			t.Fatalf("expected tag %q created on fake DO", exp)
		}
	}

	// 3. Re-run trash: idempotent!
	res2, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{
		DropletID: dID,
		Grace:     grace,
		Reason:    "stuck at boot",
		Now:       fixedNow.Add(10 * time.Minute), // Later time, but existing purge kept
	})
	if err != nil {
		t.Fatalf("re-run trash: %v", err)
	}
	if res2.Status != "noop" {
		t.Fatalf("expected status 'noop' on replay, got %q", res2.Status)
	}
	if res2.PurgeAfter != expectedPurge {
		t.Fatalf("expected existing purge %d preserved, got %d", expectedPurge, res2.PurgeAfter)
	}
	if res2.PoweredOff {
		t.Fatal("expected powered_off false on replay")
	}
}

func TestTrash_Refusals(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	// 1. Missing both lease and owner tags
	d1 := fake.AddDroplet("unowned-node", "some-other-tag")
	_, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{DropletID: d1})
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox ownership tags") {
		t.Fatalf("expected missing ownership tags error, got %v", err)
	}

	// 2. Has lease tag, but missing owner tag
	d2 := fake.AddDroplet("no-owner-node", "bashy-lease")
	_, err = c.TrashDroplet(ctx, digitalocean.TrashOptions{DropletID: d2})
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox owner tag") {
		t.Fatalf("expected missing owner tag error, got %v", err)
	}

	// 3. Has owner tag, but missing lease tag
	d3 := fake.AddDroplet("no-lease-node", "bashy-owner-worker")
	_, err = c.TrashDroplet(ctx, digitalocean.TrashOptions{DropletID: d3})
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox lease tag") {
		t.Fatalf("expected missing lease tag error, got %v", err)
	}

	// 4. Tagged permanent: must refuse!
	d4 := fake.AddDroplet("perm-node", "bashy-lease", "bashy-owner-worker", "bashy-permanent")
	_, err = c.TrashDroplet(ctx, digitalocean.TrashOptions{DropletID: d4})
	if err == nil || !strings.Contains(err.Error(), "bashy-permanent") {
		t.Fatalf("expected permanent refusal, got %v", err)
	}

	// 5. Non-existent droplet
	_, err = c.TrashDroplet(ctx, digitalocean.TrashOptions{DropletID: 99999})
	if err == nil {
		t.Fatal("expected error on non-existent droplet")
	}
}

func TestTrash_DryRun(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	dID := fake.AddDroplet("dry-sat", "bashy-lease", "bashy-owner-sat")

	res, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{
		DropletID: dID,
		Grace:     24 * time.Hour,
		Reason:    "testing dry-run",
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("trash dry-run: %v", err)
	}
	if !res.DryRun || res.Status != "planned" {
		t.Fatalf("expected planned dry-run result, got %+v", res)
	}

	// Server state must NOT have changed
	d, ok := fake.Droplet(dID)
	if !ok || d.Status != "active" {
		t.Fatalf("expected server droplet active, got %+v", d)
	}
	if len(d.Tags) != 2 {
		t.Fatalf("expected 2 original tags, got %v", d.Tags)
	}
}

func TestRestore_SuccessAndIdempotency(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	dID := fake.AddDroplet("sat-node-restore", "bashy-lease", "bashy-owner-sat")

	// First trash it
	_, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{
		DropletID: dID,
		Reason:    "maintenance",
	})
	if err != nil {
		t.Fatalf("initial trash: %v", err)
	}

	d, ok := fake.Droplet(dID)
	if !ok || d.Status != "off" {
		t.Fatalf("expected droplet off, got %+v", d)
	}

	// Now restore it
	res, err := c.RestoreDroplet(ctx, digitalocean.RestoreOptions{
		DropletID: dID,
	})
	if err != nil {
		t.Fatalf("restore droplet: %v", err)
	}
	if res.Status != "restored" {
		t.Fatalf("expected status 'restored', got %q", res.Status)
	}
	if !res.PoweredOn {
		t.Fatal("expected powered_on true")
	}

	// Verify server droplet state
	restored, ok := fake.Droplet(dID)
	if !ok {
		t.Fatal("droplet missing")
	}
	if restored.Status != "active" {
		t.Fatalf("expected status 'active', got %q", restored.Status)
	}
	for _, tag := range restored.Tags {
		if strings.HasPrefix(tag, "bashy-trash") || strings.HasPrefix(tag, "bashy-purge-after-") {
			t.Fatalf("unexpected trash tag %q remaining on restored droplet", tag)
		}
	}
	// Original lease and owner tags must still exist!
	if !slices.Contains(restored.Tags, "bashy-lease") || !slices.Contains(restored.Tags, "bashy-owner-sat") {
		t.Fatalf("original ownership tags missing: %v", restored.Tags)
	}

	// Re-run restore: idempotent!
	res2, err := c.RestoreDroplet(ctx, digitalocean.RestoreOptions{
		DropletID: dID,
	})
	if err != nil {
		t.Fatalf("re-run restore: %v", err)
	}
	if res2.Status != "noop" {
		t.Fatalf("expected status 'noop' on replay, got %q", res2.Status)
	}
	if res2.PoweredOn {
		t.Fatal("expected powered_on false on replay")
	}
}

func TestRestore_Refusals(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	// Unowned droplet
	d1 := fake.AddDroplet("foreign", "other-tag")
	_, err := c.RestoreDroplet(ctx, digitalocean.RestoreOptions{DropletID: d1})
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox ownership tags") {
		t.Fatalf("expected refusal for unowned droplet, got %v", err)
	}

	// Permanent droplet
	d2 := fake.AddDroplet("permanent", "bashy-lease", "bashy-owner-agent", "bashy-permanent")
	_, err = c.RestoreDroplet(ctx, digitalocean.RestoreOptions{DropletID: d2})
	if err == nil || !strings.Contains(err.Error(), "bashy-permanent") {
		t.Fatalf("expected refusal for permanent droplet, got %v", err)
	}
}

func TestRestore_DryRun(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	dID := fake.AddDroplet("sat-dry-restore", "bashy-lease", "bashy-owner-sat")
	_, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{DropletID: dID})
	if err != nil {
		t.Fatalf("trash: %v", err)
	}

	res, err := c.RestoreDroplet(ctx, digitalocean.RestoreOptions{
		DropletID: dID,
		DryRun:    true,
	})
	if err != nil {
		t.Fatalf("restore dry-run: %v", err)
	}
	if !res.DryRun || res.Status != "planned" {
		t.Fatalf("expected planned dry-run result, got %+v", res)
	}

	// Server state must still be off and have trash tags
	d, ok := fake.Droplet(dID)
	if !ok || d.Status != "off" {
		t.Fatalf("expected droplet still off, got %+v", d)
	}
	if !slices.Contains(d.Tags, "bashy-trash") {
		t.Fatal("expected bashy-trash tag still present on server after dry-run restore")
	}
}

func TestListTrashedDroplets(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	d1 := fake.AddDroplet("trashed-1", "bashy-lease", "bashy-owner-sat")
	d2 := fake.AddDroplet("trashed-2", "bashy-lease", "bashy-owner-sat")
	fake.AddDroplet("active-untrashed", "bashy-lease", "bashy-owner-sat")

	_, err := c.TrashDroplet(ctx, digitalocean.TrashOptions{
		DropletID: d1,
		Grace:     1 * time.Hour,
		Reason:    "failing health check",
	})
	if err != nil {
		t.Fatalf("trash d1: %v", err)
	}

	_, err = c.TrashDroplet(ctx, digitalocean.TrashOptions{
		DropletID: d2,
		Grace:     5 * time.Hour,
	})
	if err != nil {
		t.Fatalf("trash d2: %v", err)
	}

	list, err := c.ListTrashedDroplets(ctx)
	if err != nil {
		t.Fatalf("list trashed droplets: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 trashed droplets, got %d", len(list))
	}

	if list[0].DropletID != d1 || list[0].Reason != "failing-health-check" || list[0].PurgeAfter == 0 {
		t.Fatalf("unexpected d1 in list: %+v", list[0])
	}
	if list[1].DropletID != d2 || list[1].Reason != "" || list[1].PurgeAfter == 0 {
		t.Fatalf("unexpected d2 in list: %+v", list[1])
	}
}

func TestPowerOff_PowerOn_Idempotent(t *testing.T) {
	fake := fakedo.New(t)
	c := newTestClient(t, fake)
	ctx := context.Background()

	dID := fake.AddDroplet("power-test", "bashy-lease", "bashy-owner-sat")

	// Power off
	if err := c.PowerOffDroplet(ctx, dID); err != nil {
		t.Fatalf("power off: %v", err)
	}
	d, _ := fake.Droplet(dID)
	if d.Status != "off" {
		t.Fatalf("expected status 'off', got %q", d.Status)
	}

	// Power off again: noop
	if err := c.PowerOffDroplet(ctx, dID); err != nil {
		t.Fatalf("power off second time: %v", err)
	}

	// Power on
	if err := c.PowerOnDroplet(ctx, dID); err != nil {
		t.Fatalf("power on: %v", err)
	}
	d, _ = fake.Droplet(dID)
	if d.Status != "active" {
		t.Fatalf("expected status 'active', got %q", d.Status)
	}

	// Power on again: noop
	if err := c.PowerOnDroplet(ctx, dID); err != nil {
		t.Fatalf("power on second time: %v", err)
	}
}
