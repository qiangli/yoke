//go:build !darwin

package spacetime

func darwinProductVersion() (string, error) { return "", ErrNotApplicable }
