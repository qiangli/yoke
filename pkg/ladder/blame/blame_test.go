package blame

import (
	"strings"
	"testing"
	"time"
)

func validAttribution(class Class, ev []Evidence) Attribution {
	return Attribution{Class: class, Evidence: ev, By: "reviewer-1", At: time.Now()}
}

func TestParseClass(t *testing.T) {
	cases := []struct {
		in      string
		want    Class
		wantErr bool
	}{
		{"agent", ClassAgent, false},
		{"Agent", ClassAgent, false},
		{"AGENT", ClassAgent, false},
		{"environment", ClassEnvironment, false},
		{"Environment", ClassEnvironment, false},
		{"ENVIRONMENT", ClassEnvironment, false},
		{"spec", ClassSpec, false},
		{"Spec", ClassSpec, false},
		{"SPEC", ClassSpec, false},
		{"", ClassUnclassified, false},
		{"bogus", "", true},
		{"agentx", "", true},
		{" env", "", true},
		{"model", "", true},
	}
	for _, tc := range cases {
		got, err := ParseClass(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseClass(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseClass(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseClass(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidate(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name    string
		attr    Attribution
		wantErr bool
		errHas  string // substring the error must name
	}{
		{
			name:    "unclassified valid but unrated",
			attr:    Attribution{Class: ClassUnclassified, By: "reviewer-1", At: now},
			wantErr: false,
		},
		{
			name:    "unclassified empty by still valid",
			attr:    Attribution{Class: ClassUnclassified, At: now},
			wantErr: false,
		},
		{
			name:    "unclassified with stray evidence still valid",
			attr:    Attribution{Class: ClassUnclassified, Evidence: []Evidence{{Kind: "gate", Ref: "g1"}}, By: "r", At: now},
			wantErr: false,
		},
		{
			name:    "agent gate valid",
			attr:    validAttribution(ClassAgent, []Evidence{{Kind: "gate", Ref: "gate-run-1"}}),
			wantErr: false,
		},
		{
			name:    "agent review valid",
			attr:    validAttribution(ClassAgent, []Evidence{{Kind: "review", Ref: "finding-2"}}),
			wantErr: false,
		},
		{
			name:    "agent false-done valid",
			attr:    validAttribution(ClassAgent, []Evidence{{Kind: "false-done", Ref: "gate-run-9"}}),
			wantErr: false,
		},
		{
			name:    "agent missing evidence",
			attr:    validAttribution(ClassAgent, nil),
			wantErr: true,
			errHas:  "evidence",
		},
		{
			name:    "agent empty ref",
			attr:    validAttribution(ClassAgent, []Evidence{{Kind: "gate"}}),
			wantErr: true,
			errHas:  "Ref",
		},
		{
			name:    "agent wrong kind",
			attr:    validAttribution(ClassAgent, []Evidence{{Kind: "quota", Ref: "q1"}}),
			wantErr: true,
			errHas:  "Kind",
		},
		{
			name:    "agent empty by",
			attr:    Attribution{Class: ClassAgent, Evidence: []Evidence{{Kind: "gate", Ref: "g1"}}, At: now},
			wantErr: true,
			errHas:  "By",
		},
		{
			name:    "environment quota valid",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "quota", Ref: "quota-ticket-1"}}),
			wantErr: false,
		},
		{
			name:    "environment auth valid",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "auth", Ref: "auth-log-1"}}),
			wantErr: false,
		},
		{
			name:    "environment network valid",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "network", Ref: "net-log-1"}}),
			wantErr: false,
		},
		{
			name:    "environment sandbox valid",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "sandbox", Ref: "sandbox-log-1"}}),
			wantErr: false,
		},
		{
			name:    "environment hung-prompt valid",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "hung-prompt", Ref: "prompt-log-1"}}),
			wantErr: false,
		},
		{
			name:    "environment host valid",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "host", Ref: "host-log-1"}}),
			wantErr: false,
		},
		{
			name:    "environment missing evidence",
			attr:    validAttribution(ClassEnvironment, nil),
			wantErr: true,
			errHas:  "evidence",
		},
		{
			name:    "environment empty ref",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "network"}}),
			wantErr: true,
			errHas:  "Ref",
		},
		{
			name:    "environment wrong kind",
			attr:    validAttribution(ClassEnvironment, []Evidence{{Kind: "gate", Ref: "g1"}}),
			wantErr: true,
			errHas:  "Kind",
		},
		{
			name:    "environment empty by",
			attr:    Attribution{Class: ClassEnvironment, Evidence: []Evidence{{Kind: "quota", Ref: "q1"}}, At: now},
			wantErr: true,
			errHas:  "By",
		},
		{
			name:    "spec ambiguity valid",
			attr:    validAttribution(ClassSpec, []Evidence{{Kind: "ambiguity", Ref: "story-42", Note: "acceptance line unclear"}}),
			wantErr: false,
		},
		{
			name:    "spec contradiction valid",
			attr:    validAttribution(ClassSpec, []Evidence{{Kind: "contradiction", Ref: "story-43", Note: "points clash with caps"}}),
			wantErr: false,
		},
		{
			name:    "spec missing evidence",
			attr:    validAttribution(ClassSpec, nil),
			wantErr: true,
			errHas:  "evidence",
		},
		{
			name:    "spec empty ref",
			attr:    validAttribution(ClassSpec, []Evidence{{Kind: "ambiguity", Note: "unclear"}}),
			wantErr: true,
			errHas:  "Ref",
		},
		{
			name:    "spec empty note",
			attr:    validAttribution(ClassSpec, []Evidence{{Kind: "ambiguity", Ref: "story-42"}}),
			wantErr: true,
			errHas:  "Note",
		},
		{
			name:    "spec wrong kind",
			attr:    validAttribution(ClassSpec, []Evidence{{Kind: "gate", Ref: "g1", Note: "n"}}),
			wantErr: true,
			errHas:  "Kind",
		},
		{
			name:    "spec empty by",
			attr:    Attribution{Class: ClassSpec, Evidence: []Evidence{{Kind: "ambiguity", Ref: "story-1", Note: "vague"}}, At: now},
			wantErr: true,
			errHas:  "By",
		},
		{
			name:    "unknown class invalid",
			attr:    validAttribution(Class("bogus"), []Evidence{{Kind: "gate", Ref: "g1"}}),
			wantErr: true,
			errHas:  "Class",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.attr.Validate()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want error containing %q", tc.errHas)
			}
			if !strings.Contains(err.Error(), tc.errHas) {
				t.Fatalf("Validate() error %q does not name %q", err.Error(), tc.errHas)
			}
		})
	}
}

func TestConsequence(t *testing.T) {
	now := time.Now()
	by := Attribution{By: "reviewer-1", At: now}
	cases := []struct {
		name string
		attr Attribution
		want Action
	}{
		{"agent rates", Attribution{Class: ClassAgent, Evidence: []Evidence{{Kind: "gate", Ref: "g1"}}, By: by.By, At: now}, ActionRate},
		{"environment opens fix", Attribution{Class: ClassEnvironment, Evidence: []Evidence{{Kind: "network", Ref: "n1"}}, By: by.By, At: now}, ActionOpenFix},
		{"spec charges estimator and author", Attribution{Class: ClassSpec, Evidence: []Evidence{{Kind: "ambiguity", Ref: "story-1", Note: "vague"}}, By: by.By, At: now}, ActionChargeEstimatorAndAuthor},
		{"unclassified unrated", Attribution{Class: ClassUnclassified, By: by.By, At: now}, ActionUnrated},
		{"invalid agent unrated", Attribution{Class: ClassAgent, By: by.By, At: now}, ActionUnrated},
		{"invalid environment unrated", Attribution{Class: ClassEnvironment, By: by.By, At: now}, ActionUnrated},
		{"invalid spec unrated", Attribution{Class: ClassSpec, Evidence: []Evidence{{Kind: "ambiguity", Ref: "s"}}, By: by.By, At: now}, ActionUnrated},
		{"unknown class unrated", Attribution{Class: Class("bogus"), Evidence: []Evidence{{Kind: "gate", Ref: "g"}}, By: by.By, At: now}, ActionUnrated},
		{"missing by unrated", Attribution{Class: ClassAgent, Evidence: []Evidence{{Kind: "gate", Ref: "g1"}}, At: now}, ActionUnrated},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Consequence(tc.attr); got != tc.want {
				t.Errorf("Consequence() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOnlyAgentRates is the fleet evidence invariant: no rating movement
// without evidence; an unclassified failure is unrated, never scored 0.
func TestOnlyAgentRates(t *testing.T) {
	now := time.Now()
	valid := map[Class][]Evidence{
		ClassAgent:       {{Kind: "gate", Ref: "g1"}},
		ClassEnvironment: {{Kind: "quota", Ref: "q1"}},
		ClassSpec:        {{Kind: "ambiguity", Ref: "story-1", Note: "vague"}},
	}
	for class, ev := range valid {
		a := Attribution{Class: class, Evidence: ev, By: "reviewer-1", At: now}
		if err := a.Validate(); err != nil {
			t.Fatalf("class %q should validate: %v", class, err)
		}
		wantRates := class == ClassAgent
		if got := Rates(a); got != wantRates {
			t.Errorf("Rates(%q) = %v, want %v", class, got, wantRates)
		}
	}
	// Every invalid attribution must not rate, whatever its class.
	invalid := []Attribution{
		{Class: ClassUnclassified, By: "reviewer-1", At: now},
		{Class: ClassAgent, By: "reviewer-1", At: now},
		{Class: ClassEnvironment, By: "reviewer-1", At: now},
		{Class: ClassSpec, By: "reviewer-1", At: now},
		{Class: ClassAgent, Evidence: []Evidence{{Kind: "gate", Ref: "g1"}}, At: now},
		{Class: Class("bogus"), Evidence: []Evidence{{Kind: "gate", Ref: "g1"}}, By: "reviewer-1", At: now},
	}
	for i, a := range invalid {
		if Rates(a) {
			t.Errorf("invalid attribution %d (class %q) rates, want unrated", i, a.Class)
		}
		if got := Consequence(a); got != ActionUnrated {
			t.Errorf("invalid attribution %d consequence = %q, want %q", i, got, ActionUnrated)
		}
	}
}
