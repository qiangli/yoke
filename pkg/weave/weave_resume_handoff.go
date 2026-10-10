package weave

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/google/uuid"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

// Keep the complete previous attempt, including its frozen identity, launch,
// story actors and terminal evidence. History is append-only and non-recursive.
type weaveHandoffRecord struct {
	At       time.Time       `json:"at"`
	Previous json.RawMessage `json:"previous"`
}

func weaveValidateHandoff(it *weaveItem) (string, error) {
	switch it.State {
	case "killed", "failed", "allocated", "no-op":
	default:
		return "", fmt.Errorf("handoff requires a stopped run; run #%d is %s (kill/reconcile it first)", it.ID, it.State)
	}
	if it.WrapperPid > 0 && pidAlive(it.WrapperPid) {
		return "", fmt.Errorf("handoff refused: run #%d has a live wrapper", it.ID)
	}
	if it.ChildPID > 0 && pidAlive(it.ChildPID) {
		return "", fmt.Errorf("handoff refused: run #%d still has a live child; kill/reconcile it first", it.ID)
	}
	if it.ResourceReservationID != "" && !it.ResourceTerminated {
		return "", fmt.Errorf("handoff refused: child termination is unverified")
	}
	if it.ArenaSprint != 0 || it.Sealed != nil {
		return "", fmt.Errorf("handoff of arena/sealed runs is not supported")
	}
	if it.Instance != "" {
		inst, err := fleet.NewInstanceStore("").Get(it.Instance)
		if err != nil {
			return "", fmt.Errorf("read prior instance: %w", err)
		}
		if _, live := principal.InstanceOwner(inst); live {
			return "", fmt.Errorf("handoff refused: prior instance %s has a live owning session", it.Instance)
		}
	}
	if it.Workspace == "" || it.Branch == "" || it.BaseSHA == "" {
		return "", fmt.Errorf("handoff requires preserved workspace, branch and original BaseSHA")
	}
	repo, err := gogit.PlainOpen(it.Workspace)
	if err != nil {
		return "", err
	}
	head, err := repo.Head()
	if err != nil {
		return "", err
	}
	if head.Name() != plumbing.NewBranchReferenceName(it.Branch) {
		return "", fmt.Errorf("handoff workspace branch changed: %s", head.Name())
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return "", err
	}
	for _, entry := range idx.Entries {
		if entry.Stage != 0 {
			return "", fmt.Errorf("handoff workspace has unresolved index conflicts")
		}
	}
	base, err := repo.CommitObject(plumbing.NewHash(it.BaseSHA))
	if err != nil {
		return "", err
	}
	tip, err := repo.CommitObject(head.Hash())
	if err != nil {
		return "", err
	}
	ancestor, err := base.IsAncestor(tip)
	if err != nil {
		return "", err
	}
	if !ancestor {
		return "", fmt.Errorf("handoff workspace no longer descends from original BaseSHA")
	}
	return head.Hash().String(), nil
}

// Unlike ordinary resume, an explicit handoff must fail closed on identity
// errors. It never rebinds, retires, or claims the previous instance.
func weaveOpenHandoff(it *weaveItem, launch *weaveAgentLaunch, prior *weaveItem) error {
	head, err := weaveValidateHandoff(it)
	if err != nil {
		return err
	}
	if launch == nil {
		return fmt.Errorf("--handoff-to requires a registered agent")
	}
	previous := *prior
	previous.Handoffs = nil
	snapshot, err := json.Marshal(previous)
	if err != nil {
		return err
	}
	session := weaveRunSessionClaim(it.ID, it.Branch+"/handoff/"+uuid.NewString())
	ctx, err := agentlaunch.OpenContext(*launch, agentlaunch.ContextRequest{
		Fresh: true, Binding: launch.Binding(), Session: session, OwnerPID: os.Getpid(), Cwd: it.Workspace,
		Mode: "weave", Role: weaveAgentName(launch.Nick, it.ID), Task: fmt.Sprintf("handoff weave #%d: %s", it.ID, it.Title),
	})
	if err != nil {
		return fmt.Errorf("handoff verified instance: %w", err)
	}
	it.Handoffs = append(append([]weaveHandoffRecord(nil), it.Handoffs...), weaveHandoffRecord{At: time.Now().UTC(), Previous: snapshot})
	it.Instance, it.InstanceFamily, it.InstanceLabel = ctx.Instance.UUID, ctx.Instance.FamilyID, ctx.Instance.Label
	it.SessionClaim = session
	it.HandoffBaseSHA = head
	return nil
}

func weaveHandoffOptions(issue int64, tool string, args []string, o weaveStartOptions) error {
	if strings.TrimSpace(o.handoffTo) == "" {
		return nil
	}
	if !o.resume || issue <= 0 {
		return fmt.Errorf("--handoff-to requires --resume and --run N")
	}
	if tool != "" || len(args) != 0 || o.clone || o.noSpawn || o.arena != "" || o.blind || o.sealed || len(o.sealedAllow) > 0 {
		return fmt.Errorf("--handoff-to is incompatible with --tool, trailing argv, --clone, --no-spawn and arena/sealed flags")
	}
	return nil
}

// Only claims acquired by this attempt may be released on a launch failure.
// Existing claims stay with their holder; another actor's claim is a conflict,
// never implicit permission to force-take a sprint story.
func weaveHandoffNewClaims(dir string, it *weaveItem, actor string) ([]weaveWorkerStory, error) {
	stories, err := weaveResolveWorkerStories(dir, it)
	if err != nil {
		return nil, err
	}
	var pending []weaveWorkerStory
	for _, st := range stories {
		story, err := todopkg.ResolveRef(todopkg.RepoStore(st.Repo), st.ID)
		if err != nil {
			return nil, err
		}
		if story.Assignee != "" && !strings.EqualFold(story.Assignee, actor) {
			return nil, fmt.Errorf("handoff story %s is held by %s; explicitly reassign the story to %s before handing off", st.ID, story.Assignee, actor)
		}
		if story.Assignee == "" {
			st.Actor = actor
			pending = append(pending, st)
		}
	}
	return pending, nil
}

func weaveRollbackHandoffClaims(cmd *cobra.Command, claims []weaveWorkerStory) error {
	var errs []error
	quiet := &cobra.Command{}
	quiet.SetOut(io.Discard)
	quiet.SetErr(cmd.ErrOrStderr())
	for _, st := range claims {
		story, err := todopkg.ResolveRef(todopkg.RepoStore(st.Repo), st.ID)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if strings.EqualFold(story.Assignee, st.Actor) && !todopkg.IsClosed(story.Status) {
			errs = append(errs, runSprintStoryYield(quiet, st.Sprint, st.ID, st.Actor, st.Repo, "handoff failed before the replacement child started", &weaveOutputFlags{}))
		}
	}
	return errors.Join(errs...)
}
