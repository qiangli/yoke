package ask

import (
	"strings"
	"testing"
)

// `approve allow --option` picks one of the approval's offered choices (the
// browser guard's type-myself / skip / stop), refuses one it did not offer,
// and `approve show` lists them.
func TestApproveAllowOption(t *testing.T) {
	isolateApprovals(t)
	r, err := Raise(ApprovalSpec{
		Source: "browser-guard",
		Argv:   []string{"browser", "guard", "type", "x.test", "field:label-ssn"},
		Options: []ApprovalOption{
			{ID: "type-myself", Name: "I will type it myself"}, {ID: "skip", Name: "skip this question"}, {ID: "stop", Name: "stop"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := runApprove(t, "show", r.ID)
	if err != nil || !strings.Contains(out, "option:    skip — skip this question") {
		t.Fatalf("show lacks the options: %v\n%s", err, out)
	}
	if _, _, err := runApprove(t, "allow", r.ID, "--option", "fill-it"); err == nil || !strings.Contains(err.Error(), "type-myself, skip, stop") {
		t.Fatalf("an unoffered option was accepted: %v", err)
	}
	if _, _, err := runApprove(t, "allow", r.ID, "--option", "skip"); err != nil {
		t.Fatal(err)
	}
	if d, ok := LoadDecision(r.ID); !ok || !d.Allowed() || d.OptionID != "skip" {
		t.Fatalf("decision = %+v", d)
	}

	plain, _ := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"rm", "-rf", "x"}})
	if _, _, err := runApprove(t, "allow", plain.ID, "--option", "skip"); err == nil {
		t.Fatal("--option accepted on an approval that offers none")
	}
}
