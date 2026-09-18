// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package weave

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/issue"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// The bridge between the REGISTER / TODO lists and the QUEUE.
//
// `bashy issue` is the durable, committed record of what is wrong and what is wanted;
// `bashy todo` is the task backlog (per-repo or per-host);
// `bashy weave` is the per-machine queue of work an agent is actually doing. Without a
// join they would be two systems that disagree — a closed issue still "in progress" on
// someone's laptop, a merged branch whose requirement is still open. So there is
// a way for a register/todo entry to become work, linking both directions:
// the queue item remembers which item it implements (weaveItem.Register), and the
// item remembers which queue item is in flight (Issue.Weave).

func findRegisterItem(root, ref string) (*issue.Store, *issue.Issue, error) {
	if strings.TrimSpace(ref) == "" {
		return nil, nil, fmt.Errorf("empty ref")
	}
	refClean := strings.TrimPrefix(strings.TrimSpace(ref), "#")

	// 1. Repo todo store (docs/todo)
	repoTodo := todopkg.RepoStore(root)
	if it, err := todopkg.ResolveRef(repoTodo, refClean); err == nil {
		return repoTodo, it, nil
	}

	// 2. Repo issue store (.bashy/issues)
	repoIssue := &issue.Store{Root: root, Sub: issue.Dir}
	if it, err := repoIssue.Resolve(refClean); err == nil {
		return repoIssue, it, nil
	}

	// 3. User default host todo store (~/.bashy/todo/<DefaultOwner>)
	if userTodo, err := todopkg.UserStore(""); err == nil {
		if it, err := todopkg.ResolveRef(userTodo, refClean); err == nil {
			return userTodo, it, nil
		}
	}

	// 4. Any user host todo stores under ~/.bashy/todo/*
	if todoRoot, err := todopkg.Root(); err == nil {
		if entries, err := os.ReadDir(todoRoot); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					st := &issue.Store{Root: todoRoot, Sub: e.Name()}
					if it, err := todopkg.ResolveRef(st, refClean); err == nil {
						return st, it, nil
					}
				}
			}
		}
	}

	return nil, nil, fmt.Errorf("item %q not found in repo or host stores", ref)
}

func runWeaveAddFromTodo(cmd *cobra.Command, ref string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitPrecondFail, err))
	}

	st, it, err := findRegisterItem(root, ref)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitInvalidArg, err))
	}
	if it.Status == todopkg.StatusDone || it.Status == issue.StatusClosed {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitInvalidArg,
			fmt.Errorf("todo %s is already done — `bashy todo status %s doing` to reopen it first", it.ID[:min(8, len(it.ID))], it.ID[:min(8, len(it.ID))])))
	}
	if it.Weave != 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitPrecondFail,
			fmt.Errorf("todo %s is already in flight as weave #%d (`weave status %d`)", it.ID[:min(8, len(it.ID))], it.Weave, it.Weave)))
	}

	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitGenericFail, err))
	}
	prio := it.Priority
	if prio == "" {
		prio = "p2"
	}
	body := it.Body
	if len(it.Refs) > 0 {
		body += fmt.Sprintf("\n\nThis issue also touches: %v\n", it.Refs)
	}

	var qi *weaveItem
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		qi = &weaveItem{
			ID:       q.NextID,
			Title:    it.Title,
			Body:     body,
			Priority: prio,
			Stage:    it.Stage,
			Register: it.ID,
			State:    "todo",
			Created:  timeNowUTC(),
		}
		q.NextID++
		q.Items = append(q.Items, qi)
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitGenericFail, lockErr))
	}

	it.Weave = qi.ID
	if it.Status == todopkg.StatusTodo || it.Status == todopkg.StatusBlocked || it.Status == issue.StatusOpen {
		it.Status = todopkg.StatusAssigned
	}

	agent := os.Getenv("WEAVE_AGENT")
	if agent == "" {
		agent = os.Getenv("WEAVE_CONDUCTOR")
	}
	if agent == "" {
		agent = os.Getenv("USER")
	}
	it.Assignee = agent

	if _, err := st.Save(it); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitGenericFail, err))
	}

	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave add", map[string]any{
			"issue":    qi.ID,
			"todo":     it.ID,
			"register": it.ID,
			"title":    qi.Title,
			"stage":    itemStage(qi),
			"priority": qi.Priority,
			"state":    qi.State,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave add: run #%d created from todo %s (%s/%s, todo) — %q\n",
		qi.ID, it.ID[:min(8, len(it.ID))], qi.Priority, itemStage(qi), qi.Title)
	return nil
}

func runWeaveAddFromIssue(cmd *cobra.Command, ref string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitPrecondFail, err))
	}

	st, it, err := findRegisterItem(root, ref)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitInvalidArg, err))
	}
	if it.Status == todopkg.StatusDone || it.Status == issue.StatusClosed {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitInvalidArg,
			fmt.Errorf("todo %s is already done — `bashy todo --repo status %s doing` to reopen it first", it.ID[:min(8, len(it.ID))], it.ID[:min(8, len(it.ID))])))
	}
	if it.Weave != 0 {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitPrecondFail,
			fmt.Errorf("issue %s is already in flight as weave #%d (`weave status %d`)", it.ID[:min(8, len(it.ID))], it.Weave, it.Weave)))
	}

	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitGenericFail, err))
	}
	prio := it.Priority
	if prio == "" {
		prio = "p2"
	}
	body := it.Body
	if len(it.Refs) > 0 {
		body += fmt.Sprintf("\n\nThis issue also touches: %v\n", it.Refs)
	}

	var qi *weaveItem
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		qi = &weaveItem{
			ID:       q.NextID,
			Title:    it.Title,
			Body:     body,
			Priority: prio,
			Stage:    it.Stage,
			Register: it.ID,
			State:    "todo",
			Created:  timeNowUTC(),
		}
		q.NextID++
		q.Items = append(q.Items, qi)
		return nil
	})
	if lockErr != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitGenericFail, lockErr))
	}

	it.Weave = qi.ID
	if it.Status == todopkg.StatusTodo || it.Status == todopkg.StatusBlocked || it.Status == issue.StatusOpen {
		it.Status = todopkg.StatusAssigned
	}

	agent := os.Getenv("WEAVE_AGENT")
	if agent == "" {
		agent = os.Getenv("WEAVE_CONDUCTOR")
	}
	if agent == "" {
		agent = os.Getenv("USER")
	}
	it.Assignee = agent

	if _, err := st.Save(it); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave add", weavecli.ExitGenericFail, err))
	}

	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave add", map[string]any{
			"issue":    qi.ID,
			"register": it.ID,
			"title":    qi.Title,
			"stage":    itemStage(qi),
			"priority": qi.Priority,
			"state":    qi.State,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave add: run #%d created from register %s (%s/%s, todo) — %q\n",
		qi.ID, it.ID[:min(8, len(it.ID))], qi.Priority, itemStage(qi), qi.Title)
	return nil
}

// weaveCloseRegisterOnMerge settles the register/todo entry when its work lands.
//
// Called when an item reaches "done". Without it the register would drift out of date
// the moment work succeeded — and a stale register is worse than none, because people
// trust it.
func weaveCloseRegisterOnMerge(root, base string, it *weaveItem) {
	if it == nil || it.Register == "" {
		return
	}
	if it.CommitsAhead <= 0 || !weaveItemMerged(root, base, it) {
		return
	}
	st, ri, err := findRegisterItem(root, it.Register)
	if err != nil || ri.Status == todopkg.StatusDone || ri.Status == issue.StatusClosed {
		return
	}
	now := timeNowUTC()
	if st.Sub == issue.Dir {
		ri.Status = issue.StatusClosed
	} else {
		ri.Status = todopkg.StatusDone
	}
	ri.Closed = &now
	ri.Resolution = "fixed"
	ri.ClosedBy = it.Owner
	if ri.Weave == 0 {
		ri.Weave = it.ID
	}
	_, _ = st.Save(ri)
}

// weaveReleaseRegister reverts a linked todo when its run is ABANDONED — the user
// explicitly drops the work. A retryable failure/kill is NOT an abandonment (the item
// stays "assigned" to be re-run); only dropping it returns the todo to the backlog.
// Clears the link and reverts assigned -> todo, so the list never shows a stale
// "assigned" for work nobody will do.
func weaveReleaseRegister(root string, it *weaveItem) {
	if it == nil || it.Register == "" {
		return
	}
	st, ri, err := findRegisterItem(root, it.Register)
	if err != nil {
		return
	}
	changed := false
	if ri.Weave == it.ID {
		ri.Weave = 0
		changed = true
	}
	if ri.Status == todopkg.StatusAssigned || ri.Status == todopkg.StatusDoing {
		if st.Sub == issue.Dir {
			ri.Status = issue.StatusTriaged
		} else {
			ri.Status = todopkg.StatusTodo
		}
		changed = true
	}
	if changed {
		_, _ = st.Save(ri)
	}
}
