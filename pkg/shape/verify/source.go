package verify

import (
	"errors"

	gogit "github.com/go-git/go-git/v5"
)

// ErrShallowSource means the local source lacks history required by shaping's
// current clone/verification/activation transport. Shaping never fetches history
// from an upstream implicitly.
var ErrShallowSource = errors.New("shape: source has shallow history; complete local Git history is required: explicitly fetch the full history from its trusted upstream (git fetch --unshallow), preserve the selected revision, then retry")

// RequireCompleteSource enforces the shared local-transport precondition before
// candidate/tracking writes and for callers entering the verifier directly.
func RequireCompleteSource(path string) error {
	repo, e := gogit.PlainOpen(path)
	if e != nil {
		return e
	}
	shallow, e := repo.Storer.Shallow()
	if e != nil {
		return e
	}
	if len(shallow) > 0 {
		return ErrShallowSource
	}
	return nil
}
