package weave

import (
	"errors"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func TestShapeCommitRequiresMatchingTrackedIdentity(t *testing.T) {
	item := &issue.Issue{ID: "abc123def456", Origin: "shape", Shape: []issue.ShapeChange{{App: "hello", Version: "version", Actor: "agent", RepresentedUser: "person"}}}
	message := "shape: hello\n\nAdhoc: software shaping\nShape: hello@version\nShaped-By: agent for person\nShape-Item: todo:abc123def456\n"
	old := ShapeSubmissionResolver
	defer func() { ShapeSubmissionResolver = old }()
	ShapeSubmissionResolver = nil
	if err := ValidateShapeCommitMessage(message); err == nil {
		t.Fatal("missing resolver accepted")
	}
	ShapeSubmissionResolver = func(app, revision, id string) (*issue.Issue, error) {
		if app != "hello" || revision != "version" || id != item.ID {
			return nil, errors.New("unexpected identity")
		}
		return item, nil
	}
	if err := ValidateShapeCommitMessage(message); err != nil {
		t.Fatal(err)
	}
	bad := *item
	bad.Shape = []issue.ShapeChange{{App: "hello", Version: "version", Actor: "different", RepresentedUser: "person"}}
	if err := ValidateShapeCommitForItem(message, &bad); err == nil {
		t.Fatal("tampered actor accepted")
	}
	bad = *item
	bad.ID = "000000000000"
	if err := ValidateShapeCommitForItem(message, &bad); err == nil {
		t.Fatal("tampered item accepted")
	}
	if err := ValidateShapeCommitMessage("ordinary commit\n\nAdhoc: normal work\n"); err != nil {
		t.Fatal(err)
	}
}

func TestShapeOrdinaryTodoDoesNotBypassWeaveAdhocPolicy(t *testing.T) {
	item := &issue.Issue{ID: "abc123def456", Origin: "shape", Shape: []issue.ShapeChange{{App: "hello", Version: "version", Actor: "agent", RepresentedUser: "person"}}}
	message := "shape: hello\n\nAdhoc: software shaping\nShape: hello@version\nShaped-By: agent for person\nShape-Item: todo:abc123def456\n"
	t.Setenv("WEAVE_ISSUE", "")
	if err := ValidateShapeDeliveryForItem(message, item); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEAVE_ISSUE", "8")
	if err := ValidateShapeDeliveryForItem(message, item); err == nil {
		t.Fatal("shaping Adhoc bypassed normal weave policy")
	}
}

func TestShapeProposalPassesNormalHistoricalAgentAttribution(t *testing.T) {
	repo, _, baseMessage := commitAttributionFixture(t, "agent-a", true)
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	t.Setenv("BASHY_AGENT", "agent-b") // historical identity must come from commit
	store := todopkg.RepoStore(repo)
	item, err := store.Resolve("d1e86f29d7a7")
	if err != nil {
		t.Fatal(err)
	}
	item.Origin = "shape"
	item.Shape = []issue.ShapeChange{{App: "hello", Version: "version", Actor: "agent-a-w25", RepresentedUser: "person"}}
	if _, err = store.Save(item); err != nil {
		t.Fatal(err)
	}
	agent, err := ShapeAgentAttribution("agent-a-w25", item)
	if err != nil || agent != "agent-a-w25" {
		t.Fatalf("agent=%q err=%v", agent, err)
	}
	if _, err = ShapeAgentAttribution("agent-b", item); err == nil {
		t.Fatal("mismatched creator accepted")
	}
	unassigned := *item
	unassigned.Assignee = ""
	if got, err := ShapeAgentAttribution("Person A", &unassigned); err != nil || got != "" {
		t.Fatalf("person invented as agent %q %v", got, err)
	}
	message := baseMessage + "Shape: hello@version\nShaped-By: agent-a-w25 for person\nShape-Item: todo:" + item.ID + "\nAgent: " + agent + "\n"
	old := ShapeSubmissionResolver
	ShapeSubmissionResolver = func(app, revision, id string) (*issue.Issue, error) { return store.Resolve(id) }
	defer func() { ShapeSubmissionResolver = old }()
	if err = ValidateShapeDeliveryForItem(message, item); err != nil {
		t.Fatal(err)
	}
	weaveTestGit(t, repo, "commit", "--allow-empty", "-qm", message)
	out, stderr, err := commitAttributionRun("--check-range", "HEAD~1..HEAD")
	if err != nil {
		t.Fatalf("normal historical guard refused: %v %s %s", err, out, stderr)
	}
	tampered := strings.Replace(message, "Agent: "+agent+"\n", "Agent: agent-b\n", 1)
	weaveTestGit(t, repo, "commit", "--allow-empty", "-qm", tampered)
	if _, _, err = commitAttributionRun("--check-range", "HEAD~1..HEAD"); err == nil {
		t.Fatal("normal guard accepted mismatched Agent")
	}
}
