package digitalocean_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/provider/digitalocean"
	"github.com/qiangli/yoke/pkg/provider/digitalocean/fakedo"
)

func newClient(t *testing.T, fake *fakedo.Server) *digitalocean.Client {
	t.Helper()
	c, err := digitalocean.NewClient(digitalocean.ClientConfig{
		BaseURL:      fake.URL,
		Token:        fakedo.TestToken,
		PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func TestClientConfig(t *testing.T) {
	_, err := digitalocean.NewClient(digitalocean.ClientConfig{Token: ""})
	if err == nil {
		t.Fatal("expected error on empty token")
	}

	_, err = digitalocean.NewClient(digitalocean.ClientConfig{Token: "test", BaseURL: "::invalid-url"})
	if err == nil {
		t.Fatal("expected error on invalid URL")
	}
}

func TestReservedIPs(t *testing.T) {
	fake := fakedo.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	// Initially empty
	ips, err := c.ListReservedIPs(ctx)
	if err != nil {
		t.Fatalf("ListReservedIPs: %v", err)
	}
	if len(ips) != 0 {
		t.Fatalf("expected 0 reserved IPs, got %d", len(ips))
	}

	// Create with region
	rip, err := c.CreateReservedIP(ctx, digitalocean.CreateReservedIPRequest{Region: "r1"})
	if err != nil {
		t.Fatalf("CreateReservedIP: %v", err)
	}
	if rip.IP == "" || rip.Region.Slug != "r1" {
		t.Fatalf("unexpected reserved IP: %+v", rip)
	}

	// Get by IP
	got, err := c.GetReservedIP(ctx, rip.IP)
	if err != nil {
		t.Fatalf("GetReservedIP: %v", err)
	}
	if got.IP != rip.IP {
		t.Fatalf("got IP %s, want %s", got.IP, rip.IP)
	}

	// Create droplet and assign
	dID := fake.AddDroplet("sat-node", "tag1")
	if err := c.AssignReservedIP(ctx, rip.IP, dID); err != nil {
		t.Fatalf("AssignReservedIP: %v", err)
	}

	got, err = c.GetReservedIP(ctx, rip.IP)
	if err != nil {
		t.Fatalf("GetReservedIP: %v", err)
	}
	if got.Droplet == nil || got.Droplet.ID != dID {
		t.Fatalf("expected droplet %d, got %+v", dID, got.Droplet)
	}

	// Unassign
	if err := c.UnassignReservedIP(ctx, rip.IP); err != nil {
		t.Fatalf("UnassignReservedIP: %v", err)
	}

	got, err = c.GetReservedIP(ctx, rip.IP)
	if err != nil {
		t.Fatalf("GetReservedIP: %v", err)
	}
	if got.Droplet != nil {
		t.Fatalf("expected nil droplet, got %+v", got.Droplet)
	}

	// Create directly with droplet ID
	rip2, err := c.CreateReservedIP(ctx, digitalocean.CreateReservedIPRequest{DropletID: dID})
	if err != nil {
		t.Fatalf("CreateReservedIP with DropletID: %v", err)
	}
	if rip2.Droplet == nil || rip2.Droplet.ID != dID {
		t.Fatalf("expected assigned droplet %d, got %+v", dID, rip2.Droplet)
	}
}

func TestAttachReservedIP(t *testing.T) {
	fake := fakedo.New(t)
	fake.SetAsyncPolls(1)
	c := newClient(t, fake)
	ctx := context.Background()

	dID := fake.AddDroplet("sat-node-attach", "tag1")

	// 1. Attach when no IP is assigned: creates and attaches
	rip, attached, err := c.AttachReservedIP(ctx, dID, "r1")
	if err != nil {
		t.Fatalf("AttachReservedIP: %v", err)
	}
	if !attached {
		t.Fatal("expected attached=true on first call")
	}
	if rip.Droplet == nil || rip.Droplet.ID != dID {
		t.Fatalf("expected droplet %d on attached reserved IP, got %+v", dID, rip.Droplet)
	}
	if !fake.HasAction("assign", dID) {
		t.Fatalf("expected assign action to run on fake DO")
	}

	// 2. Second call: idempotent, returns existing without new action
	rip2, attached2, err := c.AttachReservedIP(ctx, dID, "r1")
	if err != nil {
		t.Fatalf("second AttachReservedIP: %v", err)
	}
	if attached2 {
		t.Fatal("expected attached=false on idempotent second call")
	}
	if rip2.IP != rip.IP {
		t.Fatalf("expected same IP %s, got %s", rip.IP, rip2.IP)
	}
	if count := fake.ActionCount("assign"); count != 1 {
		t.Fatalf("expected exactly 1 assign action across calls, got %d", count)
	}

	// 3. WaitForReservedIPAssignment verifies already-assigned IP immediately
	verified, err := c.WaitForReservedIPAssignment(ctx, rip.IP, dID)
	if err != nil {
		t.Fatalf("WaitForReservedIPAssignment: %v", err)
	}
	if verified.IP != rip.IP || verified.Droplet == nil || verified.Droplet.ID != dID {
		t.Fatalf("unexpected verified reserved IP: %+v", verified)
	}
}

func TestFirewalls(t *testing.T) {
	fake := fakedo.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	// Initially empty
	fws, err := c.ListFirewalls(ctx)
	if err != nil {
		t.Fatalf("ListFirewalls: %v", err)
	}
	if len(fws) != 0 {
		t.Fatalf("expected 0 firewalls, got %d", len(fws))
	}

	// Create firewall
	fw, err := c.CreateFirewall(ctx, &digitalocean.Firewall{
		Name:          "hardened-fw",
		InboundRules:  []digitalocean.FirewallRule{},
		OutboundRules: digitalocean.DefaultOutboundRules(),
		DropletIDs:    []int{},
		Tags:          []string{"env:test"},
	})
	if err != nil {
		t.Fatalf("CreateFirewall: %v", err)
	}
	if fw.Name != "hardened-fw" || !slices.Contains(fw.Tags, "env:test") {
		t.Fatalf("unexpected firewall: %+v", fw)
	}

	// FindFirewallByName
	found, err := c.FindFirewallByName(ctx, "hardened-fw")
	if err != nil {
		t.Fatalf("FindFirewallByName: %v", err)
	}
	if found == nil || found.ID != fw.ID {
		t.Fatalf("expected found firewall %s, got %+v", fw.ID, found)
	}

	// Add/Remove droplets
	dID := fake.AddDroplet("sat-fw", "tag1")
	if err := c.AddDropletsToFirewall(ctx, fw.ID, []int{dID}); err != nil {
		t.Fatalf("AddDropletsToFirewall: %v", err)
	}
	refreshed, _ := c.GetFirewall(ctx, fw.ID)
	if !slices.Contains(refreshed.DropletIDs, dID) {
		t.Fatalf("expected droplet %d in firewall", dID)
	}
	if err := c.RemoveDropletsFromFirewall(ctx, fw.ID, []int{dID}); err != nil {
		t.Fatalf("RemoveDropletsFromFirewall: %v", err)
	}
	refreshed, _ = c.GetFirewall(ctx, fw.ID)
	if slices.Contains(refreshed.DropletIDs, dID) {
		t.Fatalf("expected droplet %d removed from firewall", dID)
	}

	// Add/Remove tags
	if err := c.AddTagsToFirewall(ctx, fw.ID, []string{"ivy"}); err != nil {
		t.Fatalf("AddTagsToFirewall: %v", err)
	}
	refreshed, _ = c.GetFirewall(ctx, fw.ID)
	if !slices.Contains(refreshed.Tags, "ivy") {
		t.Fatalf("expected tag ivy in firewall")
	}
	if err := c.RemoveTagsFromFirewall(ctx, fw.ID, []string{"ivy"}); err != nil {
		t.Fatalf("RemoveTagsFromFirewall: %v", err)
	}
	refreshed, _ = c.GetFirewall(ctx, fw.ID)
	if slices.Contains(refreshed.Tags, "ivy") {
		t.Fatalf("expected tag ivy removed from firewall")
	}

	// Add/Remove rules
	newRule := digitalocean.FirewallRule{
		Protocol: "tcp",
		Ports:    "22",
		Sources:  &digitalocean.RuleTargets{Addresses: []string{"0.0.0.0/0", "::/0"}},
	}
	if err := c.AddRulesToFirewall(ctx, fw.ID, []digitalocean.FirewallRule{newRule}); err != nil {
		t.Fatalf("AddRulesToFirewall: %v", err)
	}
	refreshed, _ = c.GetFirewall(ctx, fw.ID)
	var hasSSH bool
	for _, r := range refreshed.InboundRules {
		if r.Matches("tcp", "22", "0.0.0.0/0") {
			hasSSH = true
			break
		}
	}
	if !hasSSH {
		t.Fatalf("expected ssh rule in firewall")
	}

	if err := c.RemoveRulesFromFirewall(ctx, fw.ID, []digitalocean.FirewallRule{newRule}); err != nil {
		t.Fatalf("RemoveRulesFromFirewall: %v", err)
	}
	refreshed, _ = c.GetFirewall(ctx, fw.ID)
	for _, r := range refreshed.InboundRules {
		if r.Matches("tcp", "22", "0.0.0.0/0") {
			t.Fatalf("expected ssh rule removed from firewall")
		}
	}
}

func TestParsePortProtocol(t *testing.T) {
	cases := []struct {
		in        string
		wantPort  string
		wantProto string
		wantErr   bool
	}{
		{"22/tcp", "22", "tcp", false},
		{"tcp/22", "22", "tcp", false},
		{"443/udp", "443", "udp", false},
		{"80", "80", "tcp", false},
		{"icmp", "all", "icmp", false},
		{"all/tcp", "all", "tcp", false},
		{"all", "all", "tcp", false},
		{"", "", "", true},
		{"80/invalid", "", "", true},
	}
	for _, tc := range cases {
		port, proto, err := digitalocean.ParsePortProtocol(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParsePortProtocol(%q): expected error, got nil", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParsePortProtocol(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if port != tc.wantPort || proto != tc.wantProto {
			t.Errorf("ParsePortProtocol(%q): got (%s, %s), want (%s, %s)", tc.in, port, proto, tc.wantPort, tc.wantProto)
		}
	}
}

func TestProjects(t *testing.T) {
	fake := fakedo.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	p := fake.AddProject("ivy-project")

	proj, err := c.FindProjectByNameOrID(ctx, "ivy-project")
	if err != nil {
		t.Fatalf("FindProjectByNameOrID: %v", err)
	}
	if proj.ID != p.ID {
		t.Fatalf("got project ID %s, want %s", proj.ID, p.ID)
	}

	dID := fake.AddDroplet("ivy-sat")
	urn := "do:droplet:" + string(rune(dID)) // URN
	urns := []string{"do:droplet:12345"}
	if err := c.AssignProjectResources(ctx, proj.ID, urns); err != nil {
		t.Fatalf("AssignProjectResources: %v", err)
	}

	res, err := c.ListProjectResources(ctx, proj.ID)
	if err != nil {
		t.Fatalf("ListProjectResources: %v", err)
	}
	if len(res) != 1 || res[0].URN != "do:droplet:12345" {
		t.Fatalf("unexpected project resources: %+v (urn: %s)", res, urn)
	}
}

func TestDropletsAndTags(t *testing.T) {
	fake := fakedo.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	dID := fake.AddDroplet("sat-tagged", "existing")

	d, err := c.GetDroplet(ctx, dID)
	if err != nil {
		t.Fatalf("GetDroplet: %v", err)
	}
	if d.Name != "sat-tagged" || !slices.Contains(d.Tags, "existing") {
		t.Fatalf("unexpected droplet: %+v", d)
	}

	if err := c.TagDroplet(ctx, "reaper:exempt", dID); err != nil {
		t.Fatalf("TagDroplet: %v", err)
	}
	if !fake.HasTag("reaper:exempt") {
		t.Fatal("expected reaper:exempt to be created on fake DO")
	}

	d, _ = c.GetDroplet(ctx, dID)
	if !slices.Contains(d.Tags, "reaper:exempt") {
		t.Fatalf("expected reaper:exempt on droplet")
	}

	if err := c.UntagDroplet(ctx, "reaper:exempt", dID); err != nil {
		t.Fatalf("UntagDroplet: %v", err)
	}

	d, _ = c.GetDroplet(ctx, dID)
	if slices.Contains(d.Tags, "reaper:exempt") {
		t.Fatalf("expected reaper:exempt removed from droplet")
	}
}

func TestCreateTag(t *testing.T) {
	fake := fakedo.New(t)
	c := newClient(t, fake)
	ctx := context.Background()

	// 1. Create brand new tag -> 201 Created
	if err := c.CreateTag(ctx, "brand-new-tag"); err != nil {
		t.Fatalf("CreateTag: %v", err)
	}
	if !fake.HasTag("brand-new-tag") {
		t.Fatal("expected brand-new-tag created on fake DO")
	}

	// 2. Create existing tag -> 422 Unprocessable Entity treated as success (nil)
	if err := c.CreateTag(ctx, "brand-new-tag"); err != nil {
		t.Fatalf("CreateTag already exists: %v", err)
	}

	// 3. Empty tag returns error
	if err := c.CreateTag(ctx, ""); err == nil {
		t.Fatal("expected error on empty tag")
	}
}
