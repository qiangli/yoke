package fleet_test

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

func runInfra(t *testing.T, fake *fakedo.Server, token string, args ...string) (string, string, error) {
	t.Helper()
	if fake != nil {
		t.Setenv("DIGITALOCEAN_API_URL", fake.URL)
	} else {
		os.Unsetenv("DIGITALOCEAN_API_URL")
	}
	if token != "" {
		t.Setenv("DIGITALOCEAN_ACCESS_TOKEN", token)
	} else {
		os.Unsetenv("DIGITALOCEAN_ACCESS_TOKEN")
	}

	cmd := fleet.NewInfraCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)

	err := cmd.Execute()
	return stdout.String(), stderr.String(), err
}

func TestInfraCredentialsRequired(t *testing.T) {
	fake := fakedo.New(t)
	// No token
	_, _, err := runInfra(t, fake, "", "ip", "list")
	if err == nil || !strings.Contains(err.Error(), "DIGITALOCEAN_ACCESS_TOKEN") {
		t.Fatalf("expected DIGITALOCEAN_ACCESS_TOKEN required error, got: %v", err)
	}

	// Verify no --access-token flag exists
	cmd := fleet.NewInfraCmd()
	for _, sub := range []string{"ip", "firewall", "harden"} {
		c, _, err := cmd.Find([]string{sub})
		if err != nil {
			t.Fatalf("subcommand %s not found: %v", sub, err)
		}
		if c.Flags().Lookup("access-token") != nil || c.PersistentFlags().Lookup("access-token") != nil {
			t.Fatalf("subcommand %s must not have --access-token flag", sub)
		}
	}
}

func TestInfraIP(t *testing.T) {
	fake := fakedo.New(t)
	dID := fake.AddDroplet("sat-node-1")

	// 1. Dry-run attach without IP
	stdout, _, err := runInfra(t, fake, fakedo.TestToken, "ip", "attach", "--droplet", "1001", "--dry-run")
	if err != nil {
		t.Fatalf("ip attach --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "[plan]") {
		t.Fatalf("expected plan output, got: %s", stdout)
	}
	// Verify nothing allocated yet
	if len(fake.ProjectResources("")) != 0 {
		t.Fatal("expected no allocation on dry-run")
	}

	// 2. Attach without IP: allocates and attaches
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "attach", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("ip attach: %v", err)
	}
	var res struct {
		DropletID int    `json:"droplet_id"`
		IP        string `json:"ip"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal attach result: %v", err)
	}
	if res.DropletID != 1001 || res.IP == "" || res.Action != "created_and_attached" {
		t.Fatalf("unexpected attach result: %+v", res)
	}
	allocatedIP := res.IP

	// 3. Attach second time: idempotent!
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "attach", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("ip attach second time: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal second attach: %v", err)
	}
	if res.Action != "already_attached" || res.IP != allocatedIP {
		t.Fatalf("expected already_attached with %s, got: %+v", allocatedIP, res)
	}

	// 4. List reserved IPs
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "list", "--json")
	if err != nil {
		t.Fatalf("ip list: %v", err)
	}
	var list []struct {
		IP      string `json:"ip"`
		Droplet *struct {
			ID int `json:"id"`
		} `json:"droplet"`
	}
	if err := json.Unmarshal([]byte(stdout), &list); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(list) != 1 || list[0].IP != allocatedIP || list[0].Droplet == nil || list[0].Droplet.ID != dID {
		t.Fatalf("unexpected list: %+v", list)
	}

	// 5. Filter list by droplet
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "list", "--droplet", "1001")
	if err != nil {
		t.Fatalf("ip list --droplet: %v", err)
	}
	if !strings.Contains(stdout, allocatedIP) {
		t.Fatalf("expected list output to contain %s, got: %s", allocatedIP, stdout)
	}

	// 6. Detach with --dry-run
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "detach", "--droplet", "1001", "--dry-run")
	if err != nil {
		t.Fatalf("ip detach --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "[plan]") {
		t.Fatalf("expected plan output for detach, got: %s", stdout)
	}

	// 7. Detach
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "detach", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("ip detach: %v", err)
	}
	var detachRes struct {
		DropletID int    `json:"droplet_id"`
		IP        string `json:"ip"`
		Action    string `json:"action"`
	}
	if err := json.Unmarshal([]byte(stdout), &detachRes); err != nil {
		t.Fatalf("unmarshal detach: %v", err)
	}
	if detachRes.Action != "detached" || detachRes.IP != allocatedIP {
		t.Fatalf("unexpected detach result: %+v", detachRes)
	}

	// Verify detach NEVER deletes the IP: IP still exists in list!
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "list", "--json")
	if err != nil {
		t.Fatalf("ip list after detach: %v", err)
	}
	var listAfter []struct {
		IP      string `json:"ip"`
		Droplet *struct {
			ID int `json:"id"`
		} `json:"droplet"`
	}
	if err := json.Unmarshal([]byte(stdout), &listAfter); err != nil {
		t.Fatalf("unmarshal list after detach: %v", err)
	}
	if len(listAfter) != 1 || listAfter[0].IP != allocatedIP {
		t.Fatalf("expected reserved IP to remain allocated, got: %+v", listAfter)
	}
	if listAfter[0].Droplet != nil {
		t.Fatalf("expected unassigned droplet, got: %+v", listAfter[0].Droplet)
	}

	// 8. Detach second time: idempotent
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "ip", "detach", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("ip detach second time: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &detachRes); err != nil {
		t.Fatalf("unmarshal detach second: %v", err)
	}
	if detachRes.Action != "already_detached" {
		t.Fatalf("expected already_detached, got: %+v", detachRes)
	}
}

func TestInfraFirewall(t *testing.T) {
	fake := fakedo.New(t)
	dID := fake.AddDroplet("sat-node-fw")

	// 1. Allow on non-existent firewall with --dry-run
	stdout, _, err := runInfra(t, fake, fakedo.TestToken, "firewall", "allow",
		"--name", "fw-hardened", "--tag", "role:satellite", "--port", "22/tcp", "--dry-run")
	if err != nil {
		t.Fatalf("firewall allow --dry-run: %v", err)
	}
	if !strings.Contains(stdout, "[plan]") {
		t.Fatalf("expected plan output, got: %s", stdout)
	}

	// 2. Allow on non-existent firewall: creates it!
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "firewall", "allow",
		"--name", "fw-hardened", "--tag", "role:satellite", "--port", "22/tcp", "--json")
	if err != nil {
		t.Fatalf("firewall allow: %v", err)
	}
	var res struct {
		Firewall string   `json:"firewall"`
		Actions  []string `json:"actions"`
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal allow: %v", err)
	}
	if res.Firewall != "fw-hardened" || !slices.Contains(res.Actions, "create_firewall") {
		t.Fatalf("unexpected allow result: %+v", res)
	}

	// Verify firewall created on fake
	fw, ok := fake.FirewallByName("fw-hardened")
	if !ok {
		t.Fatal("firewall not found on fake")
	}
	if !slices.Contains(fw.Tags, "role:satellite") {
		t.Fatalf("expected tag on firewall, got: %+v", fw.Tags)
	}

	// 3. Allow additional droplet and port 443
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "firewall", "allow",
		"--name", "fw-hardened", "--droplet", "1001", "--port", "443/tcp", "--source", "10.0.0.0/8", "--json")
	if err != nil {
		t.Fatalf("firewall allow droplet + 443: %v", err)
	}
	fw, _ = fake.FirewallByName("fw-hardened")
	if !slices.Contains(fw.DropletIDs, dID) {
		t.Fatalf("expected droplet %d in firewall", dID)
	}

	// 4. Allow again: idempotent!
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "firewall", "allow",
		"--name", "fw-hardened", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("firewall allow second time: %v", err)
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal allow second: %v", err)
	}
	if !slices.Contains(res.Actions, "already_allowed") {
		t.Fatalf("expected already_allowed, got: %+v", res)
	}

	// 5. List firewalls
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "firewall", "list", "--name", "fw-hardened")
	if err != nil {
		t.Fatalf("firewall list: %v", err)
	}
	if !strings.Contains(stdout, "fw-hardened") {
		t.Fatalf("expected list output to contain fw-hardened, got: %s", stdout)
	}

	// 6. Deny rule: removes port 22 rule but preserves firewall
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "firewall", "deny",
		"--name", "fw-hardened", "--port", "22/tcp", "--json")
	if err != nil {
		t.Fatalf("firewall deny port 22: %v", err)
	}

	// 7. Deny tag and droplet
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "firewall", "deny",
		"--name", "fw-hardened", "--tag", "role:satellite", "--droplet", "1001", "--json")
	if err != nil {
		t.Fatalf("firewall deny tag and droplet: %v", err)
	}

	// Verify firewall NEVER deleted: firewall still exists on fake!
	fw, ok = fake.FirewallByName("fw-hardened")
	if !ok {
		t.Fatal("firewall must NEVER be deleted by deny")
	}
	if slices.Contains(fw.Tags, "role:satellite") || slices.Contains(fw.DropletIDs, dID) {
		t.Fatalf("expected tag and droplet removed from firewall: %+v", fw)
	}
}

func TestInfraHarden(t *testing.T) {
	fake := fakedo.New(t)
	dID := fake.AddDroplet("sat-node-harden")
	p := fake.AddProject("ivy-project")

	// 1. Harden with --dry-run: verify plan
	stdout, _, err := runInfra(t, fake, fakedo.TestToken, "harden",
		"--droplet", "1001",
		"--project", "ivy-project",
		"--tag", "env:prod,reaper:exempt",
		"--firewall", "fw-ivy",
		"--reserved-ip",
		"--dry-run",
		"--json",
	)
	if err != nil {
		t.Fatalf("harden --dry-run: %v", err)
	}
	var dryResult fleet.HardenResult
	if err := json.Unmarshal([]byte(stdout), &dryResult); err != nil {
		t.Fatalf("unmarshal dry-run json: %v", err)
	}
	if !dryResult.DryRun || dryResult.Reconciled {
		t.Fatalf("expected dry_run=true, reconciled=false, got: %+v", dryResult)
	}
	if len(dryResult.Actions) != 4 {
		t.Fatalf("expected 4 planned actions, got %d", len(dryResult.Actions))
	}

	// Verify fake DO has NOT mutated
	d, _ := fake.Droplet(dID)
	if slices.Contains(d.Tags, "env:prod") {
		t.Fatal("dry-run must not mutate tags")
	}
	if len(fake.ProjectResources(p.ID)) != 0 {
		t.Fatal("dry-run must not assign project resources")
	}

	// 2. Harden live execution
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "harden",
		"--droplet", "1001",
		"--project", "ivy-project",
		"--tag", "env:prod,reaper:exempt",
		"--firewall", "fw-ivy",
		"--reserved-ip",
		"--json",
	)
	if err != nil {
		t.Fatalf("harden execution: %v", err)
	}
	var liveResult fleet.HardenResult
	if err := json.Unmarshal([]byte(stdout), &liveResult); err != nil {
		t.Fatalf("unmarshal live json: %v", err)
	}
	if liveResult.DryRun || !liveResult.Reconciled {
		t.Fatalf("expected dry_run=false, reconciled=true, got: %+v", liveResult)
	}

	// Verify all mutations applied to fake DO:
	// A: Droplet tags
	d, _ = fake.Droplet(dID)
	if !slices.Contains(d.Tags, "env:prod") || !slices.Contains(d.Tags, "reaper:exempt") {
		t.Fatalf("expected tags on droplet, got: %+v", d.Tags)
	}

	// B: Project membership
	projRes := fake.ProjectResources(p.ID)
	if !slices.Contains(projRes, "do:droplet:1001") {
		t.Fatalf("expected droplet in project resources, got: %+v", projRes)
	}

	// C: Firewall exists and shares tag with droplet
	fw, ok := fake.FirewallByName("fw-ivy")
	if !ok {
		t.Fatal("expected fw-ivy firewall to be created")
	}
	var sharesTag bool
	for _, t := range d.Tags {
		if slices.Contains(fw.Tags, t) {
			sharesTag = true
			break
		}
	}
	if !sharesTag {
		t.Fatalf("expected droplet tags %+v to intersect with firewall tags %+v", d.Tags, fw.Tags)
	}

	// D: Reserved IP attached and assign action executed
	if !fake.HasAction("assign", dID) {
		t.Fatalf("expected assign action for droplet %d", dID)
	}

	// 3. Reconcile second time: completely idempotent!
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "harden",
		"--droplet", "1001",
		"--project", "ivy-project",
		"--tag", "env:prod,reaper:exempt",
		"--firewall", "fw-ivy",
		"--reserved-ip",
		"--json",
	)
	if err != nil {
		t.Fatalf("harden second run: %v", err)
	}
	var secondResult fleet.HardenResult
	if err := json.Unmarshal([]byte(stdout), &secondResult); err != nil {
		t.Fatalf("unmarshal second run: %v", err)
	}
	if !secondResult.Reconciled {
		t.Fatalf("expected reconciled=true on second run, got: %+v", secondResult)
	}
	for _, a := range secondResult.Actions {
		if a.Status != "noop" {
			t.Fatalf("expected all actions to be noop on second run, got: %+v", a)
		}
	}

	// 4. Test alias --sat works identically to --droplet
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "harden",
		"--sat", "1001",
		"--project", "ivy-project",
		"--json",
	)
	if err != nil {
		t.Fatalf("harden with --sat: %v", err)
	}
}

func TestInfraHardenReservedIPAssignment(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetAsyncPolls(1)
	dID := fake.AddDroplet("sat-node-harden-ip")

	// 1. Run harden with --reserved-ip: must create, assign, and verify
	stdout, stderr, err := runInfra(t, fake, fakedo.TestToken, "harden",
		"--droplet", "1001",
		"--reserved-ip",
		"--json",
	)
	if err != nil {
		t.Fatalf("harden failed: %v, stderr: %s", err, stderr)
	}

	var res fleet.HardenResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	if !res.Reconciled {
		t.Fatalf("expected reconciled=true, got: %+v", res)
	}

	// Verify an assign action ran for the droplet on DO
	if !fake.HasAction("assign", dID) {
		t.Fatalf("expected assign action to run for droplet %d (fake has %d assign actions)", dID, fake.ActionCount("assign"))
	}

	var ipAction *fleet.HardenAction
	for _, a := range res.Actions {
		if a.Dimension == "reserved_ip" {
			ipAction = &a
			break
		}
	}
	if ipAction == nil {
		t.Fatal("expected reserved_ip action in result")
	}
	if ipAction.Status != "reconciled" {
		t.Fatalf("expected reserved_ip status reconciled, got %s", ipAction.Status)
	}

	assignedIP := ipAction.Target
	rip, ok := fake.ReservedIP(assignedIP)
	if !ok {
		t.Fatalf("reserved IP %s not found on fake DO", assignedIP)
	}
	if rip.Droplet == nil || rip.Droplet.ID != dID {
		t.Fatalf("expected reserved IP %s to be assigned to droplet %d, got: %+v", assignedIP, dID, rip.Droplet)
	}

	// 2. Second run must be idempotent noop and not execute additional assign actions
	stdout, _, err = runInfra(t, fake, fakedo.TestToken, "harden",
		"--droplet", "1001",
		"--reserved-ip",
		"--json",
	)
	if err != nil {
		t.Fatalf("second harden run failed: %v", err)
	}
	var res2 fleet.HardenResult
	if err := json.Unmarshal([]byte(stdout), &res2); err != nil {
		t.Fatalf("unmarshal second run json: %v", err)
	}
	if !res2.Reconciled {
		t.Fatalf("expected second run reconciled=true, got: %+v", res2)
	}
	for _, a := range res2.Actions {
		if a.Dimension == "reserved_ip" && a.Status != "noop" {
			t.Fatalf("expected reserved_ip status noop on second run, got %s", a.Status)
		}
	}
	if count := fake.ActionCount("assign"); count != 1 {
		t.Fatalf("expected exactly 1 assign action across runs, got %d", count)
	}
}
