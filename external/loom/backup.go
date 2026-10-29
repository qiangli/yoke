package loom

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"github.com/go-git/go-git/v5/plumbing/protocol/packp/capability"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/spf13/cobra"
)

// BackupBackend extends loom without changing callers. Sync must never delete
// or force-update a target ref. Existing tags are immutable. Divergence must
// fail with the ref name before writing refs. Implementations must protect
// updates against concurrent writers. Removing configuration never removes data.
type BackupBackend interface {
	Sync(context.Context, *git.Repository, BackupTarget) error
}

type BackupTarget struct {
	Name      string `json:"name"`
	Backend   string `json:"backend"`
	Source    string `json:"source"`
	Target    string `json:"target"`
	TokenFile string `json:"token_file,omitempty"`
}

var backupBackends = map[string]BackupBackend{
	"local":  localBackup{},
	"github": githubBackup{},
}

func backupRefs(r *git.Repository) ([]*plumbing.Reference, error) {
	it, err := r.References()
	if err != nil {
		return nil, err
	}
	defer it.Close()
	var refs []*plumbing.Reference
	err = it.ForEach(func(ref *plumbing.Reference) error {
		if ref.Type() == plumbing.HashReference && (ref.Name().IsBranch() || ref.Name().IsTag()) {
			if err := ref.Name().Validate(); err != nil {
				return err
			}
			refs = append(refs, ref)
		}
		return nil
	})
	sort.Slice(refs, func(i, j int) bool { return refs[i].Name() < refs[j].Name() })
	return refs, err
}

// Validate the entire batch before any target write, including unrelated refs.
func backupPlan(src *git.Repository, have []*plumbing.Reference) ([]*plumbing.Reference, error) {
	want, err := backupRefs(src)
	if err != nil {
		return nil, err
	}
	old := map[plumbing.ReferenceName]*plumbing.Reference{}
	for _, r := range have {
		old[r.Name()] = r
	}
	var changes []*plumbing.Reference
	for _, next := range want {
		prev := old[next.Name()]
		if prev != nil && prev.Hash() == next.Hash() && prev.Type() == plumbing.HashReference {
			continue
		}
		if prev != nil {
			ok := false
			if next.Name().IsBranch() && prev.Type() == plumbing.HashReference {
				ancestor, e1 := src.CommitObject(prev.Hash())
				descendant, e2 := src.CommitObject(next.Hash())
				if e1 == nil && e2 == nil {
					ok, err = ancestor.IsAncestor(descendant)
					if err != nil {
						return nil, err
					}
				}
			}
			if !ok {
				return nil, fmt.Errorf("backup: diverged ref %s (target %s, source %s); target unchanged", next.Name(), prev.Hash(), next.Hash())
			}
		}
		changes = append(changes, next)
	}
	return changes, nil
}

type localBackup struct{}

func (localBackup) Sync(ctx context.Context, src *git.Repository, b BackupTarget) error {
	// This directory is owned exclusively by bashy's backup adapter. A filesystem
	// lock serializes backup writers even across processes/network-mounted paths.
	if err := os.MkdirAll(filepath.Dir(b.Target), 0700); err != nil {
		return err
	}
	lock := b.Target + ".backup-lock"
	if err := os.Mkdir(lock, 0700); err != nil {
		return fmt.Errorf("backup: target locked %s: %w", b.Target, err)
	}
	defer os.Remove(lock)
	dst, err := git.PlainOpen(b.Target)
	if errors.Is(err, git.ErrRepositoryNotExists) {
		// Never initialize over someone else's directory.
		if _, e := os.Stat(b.Target); !errors.Is(e, os.ErrNotExist) {
			return fmt.Errorf("backup: target must be absent or a managed bare repository")
		}
		dst, err = git.PlainInit(b.Target, true)
		if err == nil {
			err = os.WriteFile(filepath.Join(b.Target, "bashy-backup"), []byte("never-delete\n"), 0600)
		}
	}
	if err != nil {
		return err
	}
	cfg, err := dst.Config()
	if err != nil {
		return err
	}
	if !cfg.Core.IsBare {
		return fmt.Errorf("backup: local target must be bare")
	}
	if _, err = os.Stat(filepath.Join(b.Target, "bashy-backup")); err != nil {
		return fmt.Errorf("backup: local target is not bashy-managed: %w", err)
	}
	have, err := dst.References()
	if err != nil {
		return err
	}
	var refs []*plumbing.Reference
	err = have.ForEach(func(r *plumbing.Reference) error { refs = append(refs, r); return nil })
	have.Close()
	if err != nil {
		return err
	}
	changes, err := backupPlan(src, refs)
	if err != nil || len(changes) == 0 {
		return err
	}
	// Lock every affected ref using Git's lock-file convention before copying
	// objects. A competing native Git writer cannot race even a newly created ref.
	locks := make([]string, 0, len(changes))
	defer func() {
		for _, path := range locks {
			_ = os.Remove(path)
		}
	}()
	for _, ref := range changes {
		path := filepath.Join(b.Target, filepath.FromSlash(ref.Name().String())) + ".lock"
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return fmt.Errorf("backup: lock %s: %w", ref.Name(), err)
		}
		locks = append(locks, path)
		_, err = f.WriteString(ref.Hash().String() + "\n")
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		prev, err := dst.Reference(ref.Name(), false)
		if err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
			return err
		}
		var expected *plumbing.Reference
		for _, r := range refs {
			if r.Name() == ref.Name() {
				expected = r
				break
			}
		}
		if (prev == nil) != (expected == nil) || (prev != nil && prev.String() != expected.String()) {
			return fmt.Errorf("backup: concurrent change to %s", ref.Name())
		}
	}
	objects, err := src.Storer.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return err
	}
	defer objects.Close()
	err = objects.ForEach(func(obj plumbing.EncodedObject) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if dst.Storer.HasEncodedObject(obj.Hash()) == nil {
			return nil
		}
		_, err := dst.Storer.SetEncodedObject(obj)
		return err
	})
	if err != nil {
		return err
	}
	for _, path := range locks {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := os.Rename(path, strings.TrimSuffix(path, ".lock")); err != nil {
			return err
		}
	}

	return nil
}

type githubBackup struct{}

func (githubBackup) Sync(ctx context.Context, src *git.Repository, b BackupTarget) error {
	token, err := readMirrorToken(b.TokenFile, nil)
	if err != nil {
		return err
	}
	ep, err := transport.NewEndpoint(b.Target)
	if err != nil {
		return err
	}
	cl, err := client.NewClient(ep)
	if err != nil {
		return err
	}
	session, err := cl.NewReceivePackSession(ep, &http.BasicAuth{Username: "x-access-token", Password: token})
	if err != nil {
		return err
	}
	defer session.Close()
	adv, err := session.AdvertisedReferencesContext(ctx)
	if err != nil {
		return err
	}
	if !adv.Capabilities.Supports(capability.Atomic) || !adv.Capabilities.Supports(capability.ReportStatus) {
		return fmt.Errorf("backup: github requires atomic push and report-status")
	}
	var have []*plumbing.Reference
	for name, h := range adv.References {
		have = append(have, plumbing.NewHashReference(plumbing.ReferenceName(name), h))
	}
	changes, err := backupPlan(src, have)
	if err != nil || len(changes) == 0 {
		return err
	}
	req := packp.NewReferenceUpdateRequestFromCapabilities(adv.Capabilities)
	if err := req.Capabilities.Set(capability.Atomic); err != nil {
		return err
	}
	var wants, haves []plumbing.Hash
	for _, ref := range changes {
		// The advertised old SHA (zero for absent refs) is the receive-pack CAS.
		// Unlike a second advertisement inside Push, this cannot silently change
		// the snapshot validated by backupPlan, including newly created tags.
		req.Commands = append(req.Commands, &packp.Command{Name: ref.Name(), Old: adv.References[ref.Name().String()], New: ref.Hash()})
		wants = append(wants, ref.Hash())
	}
	for _, ref := range have {
		haves = append(haves, ref.Hash())
	}
	hashes, err := revlist.Objects(src.Storer, wants, haves)
	if err != nil {
		return err
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	req.Packfile = reader
	done := make(chan error, 1)
	go func() {
		_, err := packfile.NewEncoder(writer, src.Storer, true).Encode(hashes, 0)
		_ = writer.CloseWithError(err)
		done <- err
	}()
	report, err := session.ReceivePack(ctx, req)
	_ = reader.Close() // unblock encoder on an early network failure
	encodeErr := <-done
	if err != nil {
		return err
	}
	if encodeErr != nil {
		return encodeErr
	}
	if report == nil {
		return fmt.Errorf("backup: github omitted receive-pack status")
	}
	return report.Error()
}

func validateBackup(b *BackupTarget) error {
	if b.Name == "" || strings.ContainsAny(b.Name, "/\\.") {
		return fmt.Errorf("backup: --name must be a nonempty simple name")
	}
	if backupBackends[b.Backend] == nil {
		return fmt.Errorf("backup: unsupported backend %q (local, github)", b.Backend)
	}
	if b.Source == "" || b.Target == "" {
		return fmt.Errorf("backup: --source and --target are required")
	}
	var err error
	b.Source, err = filepath.Abs(b.Source)
	if err != nil {
		return err
	}
	if b.Backend == "local" {
		b.Target, err = filepath.Abs(b.Target)
		if err != nil {
			return err
		}
		if b.Target == b.Source {
			return fmt.Errorf("backup: source and target must differ")
		}
	} else {
		u, e := url.Parse(b.Target)
		if e != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || len(strings.Split(strings.Trim(u.Path, "/"), "/")) != 2 {
			return fmt.Errorf("backup: github target must be https://github.com/owner/repo.git")
		}
		if b.TokenFile == "" || b.TokenFile == "-" {
			return fmt.Errorf("backup: github needs a persistent mode-0600 --token-file")
		}
		b.TokenFile, err = filepath.Abs(b.TokenFile)
		if err != nil {
			return err
		}
	}
	return nil
}
func syncBackup(ctx context.Context, b BackupTarget) error {
	if err := validateBackup(&b); err != nil {
		return err
	}
	src, err := git.PlainOpen(b.Source)
	if err != nil {
		return fmt.Errorf("backup: source: %w", err)
	}
	return backupBackends[b.Backend].Sync(ctx, src, b)
}
func backupDir(data string) string { return filepath.Join(data, "backups") }
func loadBackups(data string) ([]BackupTarget, error) {
	entries, err := os.ReadDir(backupDir(data))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var all []BackupTarget
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(backupDir(data), e.Name()))
		if err != nil {
			return nil, err
		}
		var b BackupTarget
		if err = json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		all = append(all, b)
	}
	return all, nil
}
func runBackups(ctx context.Context, data string, out io.Writer) {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		all, err := loadBackups(data)
		if err != nil {
			fmt.Fprintf(out, "loom backup: load: %v\n", err)
		}
		for _, b := range all {
			attempt, cancel := context.WithTimeout(ctx, 2*time.Minute)
			err := syncBackup(attempt, b)
			cancel()
			if err != nil {
				fmt.Fprintf(out, "loom backup %s FAILED: %v\n", b.Name, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func newBackupCmd() *cobra.Command {
	root := &cobra.Command{Use: "backup", Short: "Never-delete backups (local bare repository or GitHub)", Long: "Back up branches and tags automatically every 5 seconds while loom runs. Only new refs and fast-forwards; divergent refs fail loudly. rm removes configuration only. Existing Gitea push mirrors must be removed separately before migration."}
	var data string
	root.PersistentFlags().StringVar(&data, "data", DefaultDataDir(), "local loom work directory")
	var b BackupTarget
	add := &cobra.Command{Use: "add", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if err := validateBackup(&b); err != nil {
			return err
		}
		dir := backupDir(data)
		if err := os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		path := filepath.Join(dir, b.Name+".json")
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("backup: name already exists")
		}
		if err := syncBackup(c.Context(), b); err != nil {
			return err
		}
		raw, err := json.MarshalIndent(b, "", "  ")
		if err != nil {
			return err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return err
		}
		_, err = f.Write(append(raw, '\n'))
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		fmt.Fprintf(c.OutOrStdout(), "backup %s configured; initial sync complete\n", b.Name)
		return nil
	}}
	add.Flags().StringVar(&b.Name, "name", "", "backup name")
	add.Flags().StringVar(&b.Backend, "backend", "local", "local or github")
	add.Flags().StringVar(&b.Source, "source", "", "absolute path to the source loom bare git repository")
	add.Flags().StringVar(&b.Target, "target", "", "local path or GitHub repository HTTPS URL")
	add.Flags().StringVar(&b.TokenFile, "token-file", "", "persistent GitHub token file (0600)")
	list := &cobra.Command{Use: "list", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		all, err := loadBackups(data)
		if err != nil {
			return err
		}
		for _, b := range all {
			fmt.Fprintf(c.OutOrStdout(), "%s\t%s\t%s -> %s\n", b.Name, b.Backend, b.Source, b.Target)
		}
		return nil
	}}
	rm := &cobra.Command{Use: "rm NAME", Args: cobra.ExactArgs(1), RunE: func(c *cobra.Command, args []string) error {
		all, err := loadBackups(data)
		if err != nil {
			return err
		}
		for _, b := range all {
			if b.Name == args[0] {
				return os.Remove(filepath.Join(backupDir(data), b.Name+".json"))
			}
		}
		return fmt.Errorf("backup: name not found")
	}}
	sync := &cobra.Command{Use: "sync [NAME]", Args: cobra.MaximumNArgs(1), RunE: func(c *cobra.Command, args []string) error {
		all, err := loadBackups(data)
		if err != nil {
			return err
		}
		matched := false
		var failures []error
		for _, b := range all {
			if len(args) == 0 || b.Name == args[0] {
				matched = true
				if err := syncBackup(c.Context(), b); err != nil {
					failures = append(failures, fmt.Errorf("%s: %w", b.Name, err))
				} else {
					fmt.Fprintf(c.OutOrStdout(), "backup %s synced\n", b.Name)
				}
			}
		}
		if len(args) > 0 && !matched {
			return fmt.Errorf("backup: name not found")
		}
		return errors.Join(failures...)
	}}
	root.AddCommand(add, list, rm, sync)
	return root
}
