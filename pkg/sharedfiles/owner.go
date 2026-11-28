// Package sharedfiles implements the owner and peer sides of the files CLI.
package sharedfiles

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type Audience struct {
	Circles []string `json:"circles"`
	Groups  []string `json:"groups"`
	Emails  []string `json:"emails"`
}

type Record struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Mode     string    `json:"mode"`
	Audience Audience  `json:"audience"`
	Created  time.Time `json:"created"`
	Note     string    `json:"note,omitempty"`
	Forced   bool      `json:"forced,omitempty"`
	Reason   string    `json:"reason,omitempty"`
}

var nameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func checkName(name string) error {
	if !nameRE.MatchString(name) || !filepath.IsLocal(name) || strings.HasSuffix(name, ".") || strings.HasSuffix(name, ".json") {
		return fmt.Errorf("invalid share name %q", name)
	}
	return nil
}

func sharedDir() (string, error) {
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		dir, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		home = filepath.Join(dir, ".bashy")
	}
	dir, err := filepath.Abs(filepath.Join(home, "shared"))
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return dir, nil
	}
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("share record store must be a directory")
	}
	return dir, nil
}

// EnsureOffers creates the managed read-only output share with an empty
// audience. K4 may populate its audience when the offering is configured.
func EnsureOffers() (string, error) {
	dir, err := sharedDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "offers")
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("offers share must be a managed directory")
	}
	record := filepath.Join(dir, "offers.json")
	f, err := os.OpenFile(record, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		data, readErr := os.ReadFile(record)
		if readErr != nil {
			return "", readErr
		}
		var existing Record
		if json.Unmarshal(data, &existing) != nil || existing.Name != "offers" || existing.Mode != "ro" || existing.Path != path {
			return "", fmt.Errorf("offers share record conflicts with managed output share")
		}
		return path, nil
	}
	if err != nil {
		return "", err
	}
	encErr := json.NewEncoder(f).Encode(Record{Name: "offers", Path: path, Mode: "ro", Created: time.Now().UTC()})
	closeErr := f.Close()
	if encErr != nil {
		return "", encErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return path, nil
}

// contains compares existing directories by identity, including case aliases.
func contains(parent, child string) (bool, error) {
	p, err := os.Stat(parent)
	if err != nil {
		return false, err
	}
	for {
		c, err := os.Stat(child)
		if err != nil {
			return false, err
		}
		if os.SameFile(p, c) {
			return true, nil
		}
		next := filepath.Dir(child)
		if next == child {
			return false, nil
		}
		child = next
	}
}

func recordPath(dir, name, path string, forced bool) (string, error) {
	if path == "" {
		path = filepath.Join(dir, name)
		if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		if !info.IsDir() {
			return "", fmt.Errorf("managed share must be a directory")
		}
		return filepath.EvalSymlinks(path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("share path must be a directory")
	}
	if forced {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		return "", err
	}
	state, err := filepath.EvalSymlinks(filepath.Dir(dir))
	if err != nil {
		return "", err
	}
	protectedPairs := [][2]string{{path, home}, {path, state}, {state, path}}
	// An overridden BASHY_HOME can coexist with credentials in the default home.
	defaultState, err := filepath.EvalSymlinks(filepath.Join(home, ".bashy"))
	if err == nil && defaultState != state {
		protectedPairs = append(protectedPairs, [2]string{path, defaultState}, [2]string{defaultState, path})
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	for _, pair := range protectedPairs {
		protected, err := contains(pair[0], pair[1])
		if err != nil {
			return "", err
		}
		if protected {
			return "", fmt.Errorf("protected directory requires --force and --reason")
		}
	}
	return path, nil
}

func NewCmd() *cobra.Command {
	root := &cobra.Command{Use: "files", Short: "Share and read designated folders"}
	var asJSON bool
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "Print JSON")
	var r Record
	share := &cobra.Command{Use: "share NAME", Short: "Share a read-only folder", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		if err := checkName(name); err != nil {
			return err
		}
		if r.Forced != (strings.TrimSpace(r.Reason) != "") {
			return fmt.Errorf("--force and a nonempty --reason must be used together")
		}
		if cmd.Flags().Changed("path") && r.Path == "" {
			return fmt.Errorf("--path requires a directory")
		}
		dir, err := sharedDir()
		if err != nil {
			return err
		}
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		record := r
		record.Path, err = recordPath(dir, name, r.Path, r.Forced)
		if err != nil {
			return err
		}
		record.Name, record.Mode, record.Created = name, "ro", time.Now().UTC()
		tmp, err := os.CreateTemp(dir, ".share-*")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		err = json.NewEncoder(tmp).Encode(record)
		closeErr := tmp.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = os.Rename(tmp.Name(), filepath.Join(dir, name+".json")); err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(record)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), name)
		return err
	}}
	f := share.Flags()
	f.StringVar(&r.Path, "path", "", "Existing directory (default: managed folder)")
	f.StringArrayVar(&r.Audience.Circles, "circle", []string{}, "Audience circle (repeatable)")
	f.StringArrayVar(&r.Audience.Groups, "group", []string{}, "Audience group (repeatable)")
	f.StringArrayVar(&r.Audience.Emails, "email", []string{}, "Audience email (repeatable)")
	f.StringVar(&r.Note, "note", "", "Share description")
	f.BoolVar(&r.Forced, "force", false, "Allow a protected directory")
	f.StringVar(&r.Reason, "reason", "", "Reason for forcing a share")
	root.AddCommand(share, &cobra.Command{Use: "unshare NAME", Short: "Remove a share record and keep its files", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := checkName(args[0]); err != nil {
			return err
		}
		dir, err := sharedDir()
		if err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(dir, args[0]+".json")); err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"unshared": args[0]})
		}
		return nil
	}}, &cobra.Command{Use: "shares", Short: "List local shares", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, err := sharedDir()
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(dir)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		records := []Record{}
		for _, e := range entries {
			if !e.Type().IsRegular() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				return err
			}
			var record Record
			if err = json.Unmarshal(data, &record); err != nil {
				return fmt.Errorf("%s: %w", e.Name(), err)
			}
			records = append(records, record)
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(records)
		}
		for _, r := range records {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), r.Name); err != nil {
				return err
			}
		}
		return nil
	}})
	for _, verb := range []string{"ls", "stat", "cat", "get", "link"} {
		root.AddCommand(memberCmd(verb, &asJSON))
	}
	return root
}
