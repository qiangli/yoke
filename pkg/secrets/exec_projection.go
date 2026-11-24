package secrets

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var credentialAccount = regexp.MustCompile(`^[A-Za-z0-9_.@+-]+$`)

func validCredentialAccount(account string) bool {
	return account != "." && account != ".." && filepath.IsLocal(account) && credentialAccount.MatchString(account)
}

func credentialPath(name string) (provider, account, purpose string, ok bool) {
	provider, rest, _ := strings.Cut(name, "/")
	account, purpose, ok = strings.Cut(rest, "/")
	ok = ok && purpose != "" && validCredentialAccount(account)
	return
}

// RunProjected is the no-cache execution path used by secret exec. It fetches
// only the requested bindings, augments the child environment and removes its
// Google credential files when the command returns, including on failure.
// googleRoot, when set, must be an existing private scratch directory.
func (c Client) RunProjected(ctx context.Context, child *exec.Cmd, googleRoot string, names []string) (err error) {
	type projection struct {
		name, env, account string
	}
	var bindings []projection
	for _, arg := range names {
		name, local, explicit := strings.Cut(arg, "=")
		provider, account, purpose, ok := credentialPath(name)
		if !explicit && googleRoot != "" && provider == "google" && purpose == "credentials" {
			if !ok {
				return fmt.Errorf("invalid Google credential path %q", name)
			}
			bindings = append(bindings, projection{name: name, account: account})
			continue
		}
		if !explicit {
			local = strings.ToUpper(strings.NewReplacer("/", "_", "-", "_").Replace(name))
		}
		if name == "" || !validName(local) {
			return fmt.Errorf("invalid secret binding %q: use NAME=ENVVAR with a valid environment variable", arg)
		}
		bindings = append(bindings, projection{name: name, env: local})
	}

	originalEnv := child.Env
	defer func() { child.Env = originalEnv }()
	env := child.Environ()
	var root *os.Root
	for _, b := range bindings {
		value, err := c.getContext(ctx, b.name)
		if err != nil {
			return err
		}
		if b.account == "" {
			// os/exec keeps the last entry for a duplicate environment key.
			env = append(env, b.env+"="+value)
			continue
		}
		if root == nil {
			googleRoot, err = filepath.Abs(googleRoot)
			if err != nil {
				return err
			}
			root, err = os.OpenRoot(googleRoot)
			if err != nil {
				return err
			}
			defer root.Close()
			env = append(env, "GWS_CONFIG_ROOT="+googleRoot)
		}
		if err := root.Mkdir(b.account, 0o700); err != nil {
			return err
		}
		defer func() {
			if cleanupErr := root.RemoveAll(b.account); cleanupErr != nil {
				err = errors.Join(err, cleanupErr)
			}
		}()
		file, err := root.OpenFile(filepath.Join(b.account, "credentials.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, writeErr := file.WriteString(value)
		if err := errors.Join(writeErr, file.Close()); err != nil {
			return err
		}
	}
	child.Env = env
	return child.Run()
}
