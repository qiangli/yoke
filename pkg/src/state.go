package src

import (
	ygit "github.com/qiangli/yoke/git"
)

// Repo provenance states for bashy doctor and the fleet registry (M2b).
const (
	// StatePristine means local matches pristine: nothing diverged from
	// the release commit.
	StatePristine = "pristine"
	// StateModified means local diverges from pristine through skill-tier
	// paths only.
	StateModified = "modified"
	// StateModifiedProtected means the pristine..local diff touches a
	// protected (non-core) path — reachable only through an approved PR
	// merge.
	StateModifiedProtected = "modified-protected"
)

// RepoState is the provenance record State returns: the tier vocabulary
// above plus the two pins it was derived from.
type RepoState struct {
	// State is pristine, modified, or modified-protected.
	State string
	// Base is the release commit: the tip of `pristine`.
	Base string
	// Local is the tip of the host's `local` branch (or Base when local
	// does not exist yet).
	Local string
}

// State compares the host's `local` branch against `pristine` and reports
// the provenance tier: pristine when they match, modified-protected when
// the diff touches a protected (non-core) manifest path, modified
// otherwise. A core path in the diff also reports modified — core edits
// can never land through a merge, so State only observes them.
func State(home, repo string) (RepoState, error) {
	dir, err := repoDir(home, repo)
	if err != nil {
		return RepoState{}, err
	}
	base, err := ygit.ResolveCommit(dir, pristineBranch)
	if err != nil {
		return RepoState{}, err
	}
	local := base
	if ok, err := branchExists(dir, localBranch); err != nil {
		return RepoState{}, err
	} else if ok {
		if local, err = ygit.ResolveCommit(dir, localBranch); err != nil {
			return RepoState{}, err
		}
	}
	st := RepoState{State: StatePristine, Base: base, Local: local}
	if local == base {
		return st, nil
	}
	files, err := changedFiles(dir, base, local)
	if err != nil {
		return RepoState{}, err
	}
	if len(files) == 0 {
		return st, nil
	}
	st.State = StateModified
	for _, f := range files {
		if PathClass(repo, f) == ClassProtected {
			st.State = StateModifiedProtected
			break
		}
	}
	return st, nil
}
