package weave

import (
	"errors"
	"fmt"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/issue"
)

// ShapeSubmissionResolver is supplied by the shaping host. It resolves the
// submission's ordinary tracked item and checks its recorded proposal identity.
// Paths and credentials never travel in commit trailers.
var ShapeSubmissionResolver func(app, revision, itemID string) (*issue.Issue, error)

type shapeTrailers struct{ app, revision, item, by string }

func parseShapeTrailers(message string) (shapeTrailers, bool, error) {
	lines := strings.Split(strings.TrimSpace(message), "\n")
	start := len(lines) - 1
	for start >= 0 && strings.TrimSpace(lines[start]) != "" {
		start--
	}
	values := map[string][]string{}
	for _, line := range lines[start+1:] {
		m := commitTrailerLine.FindStringSubmatch(line)
		if m != nil {
			key := strings.ToLower(m[1])
			values[key] = append(values[key], strings.TrimSpace(m[2]))
		}
	}
	has := len(values["shape"])+len(values["shaped-by"])+len(values["shape-item"]) > 0
	if !has {
		return shapeTrailers{}, false, nil
	}
	for _, key := range []string{"shape", "shaped-by", "shape-item"} {
		if len(values[key]) != 1 || values[key][0] == "" {
			return shapeTrailers{}, true, fmt.Errorf("commit provenance: shaping needs exactly one %s trailer", key)
		}
	}
	app, revision, ok := strings.Cut(values["shape"][0], "@")
	if !ok || app == "" || revision == "" || strings.ContainsAny(app, "/\\ \t\n\r") || strings.ContainsAny(revision, "@ \t\n\r") {
		return shapeTrailers{}, true, fmt.Errorf("commit provenance: Shape must name APP@VERSION")
	}
	item := strings.TrimPrefix(values["shape-item"][0], "todo:")
	if !commitStoryID.MatchString(item) {
		return shapeTrailers{}, true, fmt.Errorf("commit provenance: Shape-Item needs a full todo ID")
	}
	return shapeTrailers{app: app, revision: revision, item: item, by: values["shaped-by"][0]}, true, nil
}

// ValidateShapeCommitForItem verifies link and immutable creator attribution.
// It does not replace the usual Sprint/Story or Adhoc provenance checks.
func ValidateShapeCommitForItem(message string, item *issue.Issue) error {
	fields, has, err := parseShapeTrailers(message)
	if err != nil || !has {
		return err
	}
	if item == nil || item.ID != fields.item || item.Origin != "shape" {
		return fmt.Errorf("commit provenance: shaping item does not match")
	}
	for _, change := range item.Shape {
		if change.App == fields.app && change.Version == fields.revision {
			by := change.Actor
			if change.RepresentedUser != "" && change.RepresentedUser != change.Actor {
				by += " for " + change.RepresentedUser
			}
			if fields.by != by {
				return fmt.Errorf("commit provenance: Shaped-By does not match tracked actor and represented user")
			}
			return nil
		}
	}
	return fmt.Errorf("commit provenance: Shape does not match a tracked version")
}

// ValidateShapeCommitMessage is an additive check used by commit-msg. A host
// without a submission resolver refuses shaped commits rather than trusting
// caller-provided provenance. Existing non-shaped commits are unchanged.
func ValidateShapeCommitMessage(message string) error {
	fields, has, err := parseShapeTrailers(message)
	if err != nil || !has {
		return err
	}
	if ShapeSubmissionResolver == nil {
		return fmt.Errorf("commit provenance: shaping submission resolver is unavailable")
	}
	item, err := ShapeSubmissionResolver(fields.app, fields.revision, fields.item)
	if err != nil {
		return fmt.Errorf("commit provenance: shaping link: %w", err)
	}
	return ValidateShapeCommitForItem(message, item)
}

// ValidateShapeDeliveryForItem applies the existing provenance syntax/policy
// before preparing a proposal. The eventual commit/merge still passes the full
// normal guard; this preflight cannot waive actor or manager review gates.
func ValidateShapeDeliveryForItem(message string, item *issue.Issue) error {
	if err := ValidateShapeCommitForItem(message, item); err != nil {
		return err
	}
	if item == nil {
		return fmt.Errorf("commit provenance: tracked item is missing")
	}
	if item.Sprint == 0 {
		_, ok, err := parseAdhocTrailer(message)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("commit provenance: ordinary shaping todo needs existing Adhoc provenance")
		}
		return nil
	}
	trace, err := parseCommitTrace(message)
	if err != nil {
		return err
	}
	if trace.Sprint != item.Sprint || len(trace.Stories) != 1 || trace.Stories[0].ID != item.ID || trace.Stories[0].Number != item.Seq {
		return fmt.Errorf("commit provenance: shaping story does not match normal delivery provenance")
	}
	fields, _, _ := parseShapeTrailers(message)
	for _, change := range item.Shape {
		if change.App == fields.app && change.Version == fields.revision {
			agent, err := ShapeAgentAttribution(change.Actor, item)
			if err != nil {
				return err
			}
			if agent != "" && (!trace.AgentPresent || !sprintCommitIdentityMatches(fleet.New(fleet.WithoutCloudOverlay()), agent, trace.Agent)) {
				return fmt.Errorf("commit attribution: shaped proposal lacks its registered creator Agent trailer")
			}
			break
		}
	}
	return nil
}

// ShapeAgentAttribution uses the normal registered-agent/clone resolution. A
// person is not relabeled as an agent. An assigned story must already match
// the creator; submission never changes ownership to make a guard pass.
func ShapeAgentAttribution(creator string, item *issue.Issue) (string, error) {
	cat := fleet.New(fleet.WithoutCloudOverlay())
	canonical, kind, err := cat.ResolvePrincipal(creator)
	if errors.Is(err, fleet.ErrPrincipalAmbiguous) {
		return "", fmt.Errorf("commit attribution: shaping creator is ambiguous")
	}
	agent := ""
	if err == nil && kind == fleet.KindAgent {
		agent = canonical
	}
	if item != nil && item.Sprint != 0 && item.Assignee != "" {
		if agent == "" || !sprintCommitIdentityMatches(cat, item.Assignee, agent) {
			return "", fmt.Errorf("commit attribution: shaping creator %q does not supply the registered agent attribution required by story assignee %q", creator, item.Assignee)
		}
	}
	return agent, nil
}
