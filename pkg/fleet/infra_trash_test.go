package fleet_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

func TestInfraTrash_CredentialsRequired(t *testing.T) {
	fake := fakedo.New(t)

	// No token
	_, _, err := runInfra(t, fake, "", "trash", "--droplet", "1001")
	if err == nil || !strings.Contains(err.Error(), "DIGITALOCEAN_ACCESS_TOKEN") {
		t.Fatalf("expected DIGITALOCEAN_ACCESS_TOKEN required error, got: %v", err)
	}

	_, _, err = runInfra(t, fake, "", "restore", "--droplet", "1001")
	if err == nil || !strings.Contains(err.Error(), "DIGITALOCEAN_ACCESS_TOKEN") {
		t.Fatalf("expected DIGITALOCEAN_ACCESS_TOKEN required error, got: %v", err)
	}

	// Verify no --access-token flag exists
	cmd := fleet.NewInfraCmd()
	for _, sub := range []string{"trash", "restore"} {
		c, _, err := cmd.Find([]string{sub})
		if err != nil {
			t.Fatalf("subcommand %s not found: %v", sub, err)
		}
		if c.Flags().Lookup("access-token") != nil || c.PersistentFlags().Lookup("access-token") != nil {
			t.Fatalf("subcommand %s must not have --access-token flag", sub)
		}
	}
}

func TestInfraTrash_Refusals(t *testing.T) {
	fake := fakedo.New(t)

	// 1. Missing --droplet ID
	_, _, err := runInfra(t, fake, fakedo.TestToken, "trash")
	if err == nil || !strings.Contains(err.Error(), "--droplet ID is required") {
		t.Fatalf("expected missing droplet ID error, got: %v", err)
	}

	// 2. Unowned droplet (missing bashy-lease and bashy-owner-*)
	d1 := fake.AddDroplet("foreign-node")
	_, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1001")
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox ownership tags") {
		t.Fatalf("expected missing ownership error, got: %v", err)
	}

	// 3. Missing bashy-owner-*
	_ = fake.AddDroplet("no-owner-node", "bashy-lease")
	_, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1002")
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox owner tag") {
		t.Fatalf("expected missing owner error, got: %v", err)
	}

	// 4. Missing bashy-lease
	_ = fake.AddDroplet("no-lease-node", "bashy-owner-sat")
	_, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1003")
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox lease tag") {
		t.Fatalf("expected missing lease error, got: %v", err)
	}

	// 5. Permanent droplet: bashy-permanent
	dPerm := fake.AddDroplet("perm-node", "bashy-lease", "bashy-owner-sat", "bashy-permanent")
	_, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1004")
	if err == nil || !strings.Contains(err.Error(), "bashy-permanent") {
		t.Fatalf("expected permanent refusal, got: %v", err)
	}

	// Ensure d1 and dPerm status did not change
	d, _ := fake.Droplet(d1)
	if d.Status != "active" {
		t.Fatalf("d1 status modified: %s", d.Status)
	}
	dp, _ := fake.Droplet(dPerm)
	if dp.Status != "active" {
		t.Fatalf("dPerm status modified: %s", dp.Status)
	}
}

func TestInfraTrash_Lifecycle(t *testing.T) {
	fake := fakedo.New(t)
	dID := fake.AddDroplet("sat-node-1", "bashy-lease", "bashy-owner-sat")

	// 1. Dry-run trash
	stdout, _, err := runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1001", "--grace", "12h", "--reason", "stuck at boot", "--dry-run")
	if err != nil {
		t.Fatalf("trash dry-run failed: %v", err)
	}
	if !strings.Contains(stdout, "[plan]") || !strings.Contains(stdout, "bashy-trash") {
		t.Fatalf("expected plan in dry-run output, got: %s", stdout)
	}
	// Verify server droplet unchanged
	d, ok := fake.Droplet(dID)
	if !ok || d.Status != "active" || slices.Contains(d.Tags, "bashy-trash") {
		t.Fatalf("droplet was mutated during dry-run: %+v", d)
	}

	// 2. Real trash with JSON
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1001", "--grace", "12h", "--reason", "stuck at boot", "--json")
	if err != nil {
		t.Fatalf("trash failed: %v", err)
	}
	var res struct {
		DropletID  int      `json:"droplet_id"`
		DryRun     bool     `json:"dry_run"`
		Status     string   `json:"status"`
		PurgeAfter int64    `json:"purge_after"`
		Reason     string   `json:"reason"`
		TagsAdded  []string `json:"tags_added"`
		PoweredOff bool     `json:"powered_off"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal trash JSON: %v, raw: %s", err, stdout)
	}
	if res.DropletID != 1001 || res.Status != "trashed" || res.Reason != "stuck-at-boot" || res.PurgeAfter == 0 || !res.PoweredOff {
		t.Fatalf("unexpected trash result: %+v", res)
	}

	// Verify server droplet state
	d, ok = fake.Droplet(dID)
	if !ok {
		t.Fatal("droplet missing")
	}
	if d.Status != "off" {
		t.Fatalf("expected droplet off, got %s", d.Status)
	}
	if !slices.Contains(d.Tags, "bashy-trash") {
		t.Fatal("missing bashy-trash tag")
	}
	if !slices.Contains(d.Tags, "bashy-trash-reason-stuck-at-boot") {
		t.Fatal("missing bashy-trash-reason-stuck-at-boot tag")
	}

	// 3. Re-run trash: idempotent!
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("re-run trash failed: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal replay JSON: %v", err)
	}
	if res.Status != "noop" {
		t.Fatalf("expected status 'noop' on replay, got %s", res.Status)
	}

	// 4. Trash list (ls)
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "ls", "--json")
	if err != nil {
		t.Fatalf("trash ls failed: %v", err)
	}
	var list []struct {
		DropletID int    `json:"droplet_id"`
		Name      string `json:"name"`
		Status    string `json:"status"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("unmarshal ls JSON: %v, raw: %s", err, stdout)
	}
	if len(list) != 1 || list[0].DropletID != 1001 || list[0].Reason != "stuck-at-boot" {
		t.Fatalf("unexpected list output: %+v", list)
	}

	// Text ls output
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "ls")
	if err != nil {
		t.Fatalf("trash ls text failed: %v", err)
	}
	if !strings.Contains(stdout, "1001") || !strings.Contains(stdout, "stuck-at-boot") {
		t.Fatalf("expected 1001 and reason in text ls, got: %s", stdout)
	}

	// 5. Restore dry-run
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "restore", "--droplet", "1001", "--dry-run")
	if err != nil {
		t.Fatalf("restore dry-run failed: %v", err)
	}
	if !strings.Contains(stdout, "[plan]") {
		t.Fatalf("expected plan in restore dry-run, got: %s", stdout)
	}
	d, _ = fake.Droplet(dID)
	if d.Status != "off" {
		t.Fatalf("expected droplet still off after dry-run restore, got %s", d.Status)
	}

	// 6. Real restore with JSON
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "restore", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	var restRes struct {
		DropletID   int      `json:"droplet_id"`
		Status      string   `json:"status"`
		TagsRemoved []string `json:"tags_removed"`
		PoweredOn   bool     `json:"powered_on"`
	}
	if err := json.Unmarshal([]byte(stdout), &restRes); err != nil {
		t.Fatalf("unmarshal restore JSON: %v", err)
	}
	if restRes.DropletID != 1001 || restRes.Status != "restored" || !restRes.PoweredOn {
		t.Fatalf("unexpected restore result: %+v", restRes)
	}

	// Verify server droplet state after restore
	d, _ = fake.Droplet(dID)
	if d.Status != "active" {
		t.Fatalf("expected droplet active after restore, got %s", d.Status)
	}
	if slices.Contains(d.Tags, "bashy-trash") {
		t.Fatal("bashy-trash tag still present after restore")
	}

	// 7. Trash ls is now empty
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "trash", "ls", "--json")
	if err != nil {
		t.Fatalf("trash ls after restore failed: %v", err)
	}
	var emptyList []any
	if err := json.Unmarshal([]byte(stdout), &emptyList); err != nil {
		t.Fatalf("unmarshal empty list: %v", err)
	}
	if len(emptyList) != 0 {
		t.Fatalf("expected empty list after restore, got: %+v", emptyList)
	}
}

func TestInfraRestore_Refusals(t *testing.T) {
	fake := fakedo.New(t)

	// Missing --droplet
	_, _, err := runInfra(t, fake, fakedo.TestToken, "restore")
	if err == nil || !strings.Contains(err.Error(), "--droplet ID is required") {
		t.Fatalf("expected missing droplet ID error, got: %v", err)
	}

	// Unowned droplet
	_ = fake.AddDroplet("foreign-node")
	_, _, err = runInfra(t, fake, fakedo.TestToken, "restore", "--droplet", "1001")
	if err == nil || !strings.Contains(err.Error(), "missing cloudbox ownership tags") {
		t.Fatalf("expected missing ownership error, got: %v", err)
	}

	// Permanent droplet
	_ = fake.AddDroplet("perm-node", "bashy-lease", "bashy-owner-sat", "bashy-permanent")
	_, _, err = runInfra(t, fake, fakedo.TestToken, "restore", "--droplet", "1002")
	if err == nil || !strings.Contains(err.Error(), "bashy-permanent") {
		t.Fatalf("expected permanent refusal, got: %v", err)
	}
}
