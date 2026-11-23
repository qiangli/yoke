package skills

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const skillIndexPath = "/.well-known/agent-skills/index.json"

var errSkillURLMissing = errors.New("remote skill resource not found")

// stageURLSkill resolves a site or skill URL and stages one SKILL.md for addFolder.
func stageURLSkill(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fmt.Errorf("skills: expected an HTTP(S) URL without credentials")
	}
	client := &http.Client{
		Timeout: 15 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
				return fmt.Errorf("skills: refused download redirect")
			}
			if via[len(via)-1].URL.Scheme == "https" && req.URL.Scheme != "https" {
				return fmt.Errorf("skills: refused download redirect from HTTPS to HTTP")
			}
			return nil
		},
	}
	get := func(u *url.URL) ([]byte, *url.URL, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return nil, nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, nil, err
		}
		defer resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusOK:
		case http.StatusNotFound, http.StatusGone:
			return nil, nil, fmt.Errorf("skills: %s: %w", u, errSkillURLMissing)
		default:
			return nil, nil, fmt.Errorf("skills: %s: HTTP %d", u, resp.StatusCode)
		}
		const limit = 2 << 20
		data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		if len(data) > limit {
			return nil, nil, fmt.Errorf("skills: %s exceeds the download size limit", u)
		}
		return data, resp.Request.URL, err
	}

	target := u
	var name string
	var digest *string
	if !strings.EqualFold(path.Base(u.Path), "skill.md") {
		indexURL := u
		explicit := u.Path == skillIndexPath
		if !explicit {
			indexURL = u.ResolveReference(&url.URL{Path: skillIndexPath})
		}
		data, base, err := get(indexURL)
		switch {
		case err == nil:
			var index struct {
				Version string
				Skills  []struct {
					Name, Type, URL string
					SHA256          *string
				}
			}
			if err := json.Unmarshal(data, &index); err != nil {
				return "", fmt.Errorf("skills: invalid index: %w", err)
			}
			if index.Version != "0.2.0" {
				return "", fmt.Errorf("skills: unsupported index version %q", index.Version)
			}
			target = nil
			for _, entry := range index.Skills {
				if entry.Type != "skill-md" {
					continue
				}
				if target != nil {
					return "", fmt.Errorf("skills: index has multiple skill-md entries; select a direct skill URL")
				}
				if entry.URL == "" {
					return "", fmt.Errorf("skills: index entry has no URL")
				}
				target, err = base.Parse(entry.URL)
				if err != nil || target.Hostname() == "" || target.User != nil || (target.Scheme != "http" && target.Scheme != "https") {
					return "", fmt.Errorf("skills: index entry must resolve to an HTTP(S) URL without credentials")
				}
				name, digest = entry.Name, entry.SHA256
			}
			if target == nil {
				return "", fmt.Errorf("skills: index has no skill-md entry")
			}
		case !explicit && errors.Is(err, errSkillURLMissing):
			target = u.ResolveReference(&url.URL{Path: "/skill.md"})
		default:
			return "", err
		}
	}
	data, _, err := get(target)
	if err != nil {
		return "", err
	}
	if digest != nil && !strings.EqualFold(*digest, fmt.Sprintf("%x", sha256.Sum256(data))) {
		return "", fmt.Errorf("skills: SHA-256 mismatch for %s", target)
	}
	sk, err := ParseFrontmatter(data)
	if err != nil {
		return "", err
	}
	if err := validSkillName(sk.Name); err != nil {
		return "", err
	}
	if name != "" && sk.Name != name {
		return "", fmt.Errorf("skills: index name %q differs from SKILL.md name %q", name, sk.Name)
	}
	dir, err := os.MkdirTemp("", "skill-url-*")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), data, 0600); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// A repeated remote add can recover a caller that finished installing the
// skill before its next step failed. It never replaces a different bundle.
func installedURLSkillMatches(staged, installed string) bool {
	info, err := os.Lstat(installed)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(installed)
	if err != nil || len(entries) != 1 || entries[0].Name() != skillMarker || !entries[0].Type().IsRegular() {
		return false
	}
	source, err := os.ReadFile(filepath.Join(staged, skillMarker))
	if err != nil {
		return false
	}
	body, err := os.ReadFile(filepath.Join(installed, skillMarker))
	return err == nil && bytes.Equal(source, body)
}
