package weave

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
)

type boothTemplate struct {
	Dir    string
	Digest string
}

// BoothTemplateDir is the on-disk location of a story's reusable template.
func BoothTemplateDir(sprint int, story string) string {
	home := os.Getenv("BASHY_HOME")
	if home == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			home = filepath.Join(userHome, ".bashy")
		}
	}
	return filepath.Join(home, "sprint", "booths", strconv.Itoa(sprint), story, "template")
}

func buildBoothTemplate(ctx context.Context, sprint int, story string, src string, setup []string) (boothTemplate, error) {
	if sprint < 0 || story == "" || filepath.Base(story) != story || story == "." || story == ".." {
		return boothTemplate{}, fmt.Errorf("invalid booth template location")
	}
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return boothTemplate{}, fmt.Errorf("template source %q: %w", src, err)
	}
	sourceDigest, err := boothTemplateDigest(src)
	if err != nil {
		return boothTemplate{}, fmt.Errorf("digest template source: %w", err)
	}
	dir := BoothTemplateDir(sprint, story)
	parent := filepath.Dir(dir)
	metadata := filepath.Join(parent, "template.json")
	inputs := strings.Join(setup, "\x00")
	var prior struct {
		Source string `json:"source"`
		Setup  string `json:"setup"`
		Digest string `json:"digest"`
	}
	if data, readErr := os.ReadFile(metadata); readErr == nil && json.Unmarshal(data, &prior) == nil && prior.Source == sourceDigest && prior.Setup == inputs {
		if current, digestErr := boothTemplateDigest(dir); digestErr == nil && current == prior.Digest {
			return boothTemplate{Dir: dir, Digest: current}, nil
		}
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return boothTemplate{}, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return boothTemplate{}, err
	}
	if _, _, err := copyBoothTemplate(boothTemplate{Dir: src}, dir); err != nil {
		return boothTemplate{}, fmt.Errorf("copy template source: %w", err)
	}
	log, err := os.OpenFile(filepath.Join(parent, "setup.log"), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return boothTemplate{}, err
	}
	for _, command := range setup {
		runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cmd := exec.CommandContext(runCtx, "bash", "-c", command)
		cmd.Dir = dir
		cmd.Stdout = log
		cmd.Stderr = log
		runErr := cmd.Run()
		cancel()
		if runErr != nil {
			_ = log.Close()
			return boothTemplate{}, fmt.Errorf("template setup failed; see %s: %w", log.Name(), runErr)
		}
	}
	if err := log.Close(); err != nil {
		return boothTemplate{}, err
	}
	digest, err := boothTemplateDigest(dir)
	if err != nil {
		return boothTemplate{}, err
	}
	data, err := json.Marshal(struct {
		Source string `json:"source"`
		Setup  string `json:"setup"`
		Digest string `json:"digest"`
	}{sourceDigest, inputs, digest})
	if err != nil {
		return boothTemplate{}, err
	}
	if err := os.WriteFile(metadata, data, 0600); err != nil {
		return boothTemplate{}, err
	}
	return boothTemplate{Dir: dir, Digest: digest}, nil
}

// copyBoothTemplate creates a new attempt and reports the file-copy method and elapsed time.
func copyBoothTemplate(template boothTemplate, dst string) (string, time.Duration, error) {
	started := time.Now()
	if template.Dir == "" {
		return "", 0, fmt.Errorf("empty booth template")
	}
	if _, err := os.Lstat(dst); err == nil {
		return "", 0, fmt.Errorf("booth destination %q already exists", dst)
	} else if !os.IsNotExist(err) {
		return "", 0, err
	}
	methods := map[string]bool{}
	err := filepath.WalkDir(template.Dir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(template.Dir, path)
		if err != nil {
			return err
		}
		target := dst
		if rel != "." {
			target = filepath.Join(dst, rel)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case entry.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		case info.Mode().IsRegular():
			method, err := boothCloneFile(path, target, info.Mode())
			if err == nil {
				methods[method] = true
			}
			return err
		default:
			return fmt.Errorf("unsupported template file %q", path)
		}
	})
	if err != nil {
		return "", time.Since(started), err
	}
	reported := make([]string, 0, len(methods))
	for method := range methods {
		reported = append(reported, method)
	}
	sort.Strings(reported)
	if len(reported) == 0 {
		reported = append(reported, "copy")
	}
	return strings.Join(reported, "+"), time.Since(started), nil
}

// boothTemplateDigest excludes .git/objects, whose pack representation is not
// stable. For a Git checkout it additionally records HEAD's tracked tree hash.
func boothTemplateDigest(root string) (string, error) {
	var records []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git/objects" || strings.HasPrefix(rel, ".git/objects/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.Copy(hash, file)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		records = append(records, rel+"\x00"+strconv.FormatInt(info.Size(), 10)+"\x00"+hex.EncodeToString(hash.Sum(nil)))
		return nil
	})
	if err != nil {
		return "", err
	}
	if repo, err := gogit.PlainOpen(root); err == nil {
		if head, err := repo.Head(); err == nil {
			if commit, err := repo.CommitObject(head.Hash()); err == nil {
				records = append(records, ".git/tracked-tree\x00"+commit.TreeHash.String())
			}
		}
	}
	sort.Strings(records)
	hash := sha256.Sum256([]byte(strings.Join(records, "\n")))
	return hex.EncodeToString(hash[:]), nil
}

func dropBoothTemplates(sprint int) error {
	if sprint < 0 {
		return fmt.Errorf("invalid sprint %d", sprint)
	}
	return os.RemoveAll(filepath.Dir(filepath.Dir(BoothTemplateDir(sprint, "x"))))
}
