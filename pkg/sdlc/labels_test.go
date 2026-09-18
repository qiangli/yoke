package sdlc

import "testing"

func TestEligibleByLabels(t *testing.T) {
	cases := []struct {
		name            string
		labels          []string
		title           string
		requireInitiate bool
		want            bool
	}{
		{"plain open issue, loom-style", nil, "fix thing", false, true},
		{"empty title never eligible", []string{"sdlc:go"}, "  ", true, false},
		{"reserved skip: in-progress", []string{"sdlc:in-progress"}, "x", false, false},
		{"reserved skip: blocked", []string{"sdlc:blocked"}, "x", false, false},
		{"reserved skip wins over initiate", []string{"sdlc:go", "sdlc:qa"}, "x", true, false},
		{"requireInitiate without sdlc:go", []string{"type:bug"}, "x", true, false},
		{"requireInitiate with sdlc:go", []string{"sdlc:go", "type:bug"}, "x", true, true},
		{"case/space insensitive", []string{"  SDLC:GO "}, "x", true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eligibleByLabels(c.labels, c.title, c.requireInitiate); got != c.want {
				t.Fatalf("eligibleByLabels(%v, %q, %v) = %v, want %v", c.labels, c.title, c.requireInitiate, got, c.want)
			}
		})
	}
}

func TestPriorityByLabels(t *testing.T) {
	cases := []struct {
		labels []string
		want   int
	}{
		{nil, 100},
		{[]string{"priority:p0"}, 0},
		{[]string{"priority:p2", "priority:p1"}, 1}, // lowest number wins
		{[]string{"type:bug", "priority:p3"}, 3},
		{[]string{"PRIORITY:P0"}, 0}, // case-insensitive
	}
	for _, c := range cases {
		if got := priorityByLabels(c.labels); got != c.want {
			t.Fatalf("priorityByLabels(%v) = %d, want %d", c.labels, got, c.want)
		}
	}
}

func TestDeployLabelRoundTrip(t *testing.T) {
	for _, env := range []string{"dev", "qa", "prod"} {
		label := DeployLabelForEnv(env)
		if label != "deploy:"+env {
			t.Fatalf("DeployLabelForEnv(%q) = %q", env, label)
		}
		if got := EnvFromDeployLabel(label); got != env {
			t.Fatalf("EnvFromDeployLabel(%q) = %q, want %q", label, got, env)
		}
	}
	if got := EnvFromDeployLabel("sdlc:go"); got != "" {
		t.Fatalf("EnvFromDeployLabel(non-deploy) = %q, want empty", got)
	}
	if got := EnvFromDeployLabel("DEPLOY:Prod"); got != "prod" {
		t.Fatalf("EnvFromDeployLabel case-insensitive = %q", got)
	}
}

func TestReservedLabelsCoverage(t *testing.T) {
	got := map[string]bool{}
	for _, l := range ReservedLabels() {
		got[l] = true
	}
	for _, must := range []string{LabelInitiate, LabelInProgress, LabelDone, "deploy:qa", "deploy:prod", "priority:p0", "type:bug"} {
		if !got[must] {
			t.Fatalf("ReservedLabels() missing %q", must)
		}
	}
}
