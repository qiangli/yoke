package ladder

import "testing"

func TestSimulatePlanDeterministicAndBetter(t *testing.T) {
	base := []PlanStep{{Story: "one", Points: 3, StoryRating: 1500, Agent: Rating{R: 1300, RD: 50}}, {Story: "two", Points: 5, StoryRating: 1500, Agent: Rating{R: 1300, RD: 50}, DependsOn: []string{"one"}}}
	a, err := SimulatePlan(base, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := SimulatePlan(base, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("nondeterministic: %+v %+v", a, b)
	}
	better := append([]PlanStep(nil), base...)
	better[0].Agent.R, better[1].Agent.R = 1800, 1800
	c, err := SimulatePlan(better, 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if c.ExpectedPoints <= a.ExpectedPoints {
		t.Fatalf("better plan: %+v <= %+v", c, a)
	}
	d, err := ComparePlans(better, base, 10, 2)
	if err != nil || d.ExpectedPoints <= 0 {
		t.Fatalf("paired difference: %+v %v", d, err)
	}
}

func TestSimulatePlanDependencies(t *testing.T) {
	plan := []PlanStep{{Story: "one", Points: 3, StoryRating: 1000, Agent: Rating{R: 2200, RD: 30}}, {Story: "two", Points: 3, StoryRating: 1000, Agent: Rating{R: 2200, RD: 30}, DependsOn: []string{"one"}}}
	with, err := SimulatePlan(plan, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	plan[1].DependsOn = nil
	without, err := SimulatePlan(plan, 20, 2)
	if err != nil {
		t.Fatal(err)
	}
	if with.ExpectedMakespan <= without.ExpectedMakespan {
		t.Fatalf("dependency did not delay: %+v %+v", with, without)
	}
	plan[1].DependsOn = []string{"missing"}
	if _, err := SimulatePlan(plan, 20, 2); err == nil {
		t.Fatal("unknown dependency accepted")
	}
}
