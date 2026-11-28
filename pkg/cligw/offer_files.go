package cligw

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/sharedfiles"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

// preparePeerFiles resolves only designated files. The scratch directory is
// private to this run and removed after the response has been assembled.
func preparePeerFiles(ctx context.Context, args string, opts toolcmd.Options) (string, toolcmd.Options, func(), error) {
	dir, err := os.MkdirTemp("", "bashy-offer-*")
	if err != nil {
		return args, opts, nil, err
	}
	release := func() { _ = os.RemoveAll(dir) }
	words := strings.Fields(args)
	if split, err := toolcmd.SplitArgs(args); err == nil {
		words = split
	}
	if _, err := os.Stat(args); err == nil {
		release()
		return args, opts, nil, fmt.Errorf("host paths are refused for peer tool calls; use a bashy://HOST/files-NAME/PATH URI")
	}
	if len(words) == 0 && args != "" {
		words = []string{args}
	}
	for i, word := range words {
		candidate := strings.Trim(word, "\"'(),")
		prefix := ""
		if j := strings.IndexByte(candidate, '='); j >= 0 {
			prefix, candidate = candidate[:j+1], candidate[j+1:]
		}
		if strings.HasPrefix(candidate, "bashy://") {
			target, parseErr := sharedfiles.ParseTarget(candidate)
			if parseErr != nil || target.Path == "" {
				release()
				return args, opts, nil, fmt.Errorf("invalid bashy:// files URI")
			}
			dst := filepath.Join(dir, fmt.Sprintf("input-%d-%s", i, filepath.Base(target.Path)))
			if err := sharedfiles.Fetch(ctx, candidate, dst); err != nil {
				release()
				return args, opts, nil, err
			}
			args = strings.Replace(args, prefix+candidate, prefix+dst, 1)
			continue
		}
		if candidate == "" {
			continue
		}
		if strings.HasPrefix(candidate, "file://") {
			release()
			return args, opts, nil, fmt.Errorf("host paths are refused for peer tool calls; use a bashy://HOST/files-NAME/PATH URI")
		}
		if _, err := os.Stat(candidate); err == nil {
			release()
			return args, opts, nil, fmt.Errorf("host paths are refused for peer tool calls; use a bashy://HOST/files-NAME/PATH URI")
		}
	}
	opts.Dir = dir
	if opts.Argv != nil {
		opts.Argv = []string{args}
	}
	return args, opts, release, nil
}

func offerHost() (string, error) {
	host, err := os.Hostname()
	if err != nil {
		return "", err
	}
	host = strings.ToLower(host)
	if host == "" || strings.ContainsAny(host, "/\\:") {
		return "", fmt.Errorf("invalid offerer hostname")
	}
	return host, nil
}

func publishPeerArtifacts(res toolcmd.Result, runErr error) (toolcmd.Result, error) {
	artifacts := res.Artifacts
	res.Artifacts = nil
	res.Dir = ""
	res.Argv = nil
	res.Slash = ""
	res.Steps = nil
	if len(artifacts) == 0 {
		return res, runErr
	}
	root, err := sharedfiles.EnsureOffers()
	if err != nil {
		return res, fmt.Errorf("publish artifacts: %w", err)
	}
	host, err := offerHost()
	if err != nil {
		return res, fmt.Errorf("publish artifacts: %w", err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return res, err
	}
	runID := hex.EncodeToString(nonce[:])
	dir := filepath.Join(root, runID)
	if err := os.Mkdir(dir, 0700); err != nil {
		return res, err
	}
	for i, source := range artifacts {
		in, err := os.Open(source)
		if err != nil {
			return res, fmt.Errorf("publish artifact: %w", err)
		}
		info, err := in.Stat()
		if err != nil || !info.Mode().IsRegular() {
			in.Close()
			return res, fmt.Errorf("publish artifact: source is not a regular file")
		}
		name := fmt.Sprintf("%d-%s", i, filepath.Base(source))
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			in.Close()
			return res, err
		}
		hash := sha256.New()
		size, copyErr := io.Copy(io.MultiWriter(out, hash), in)
		closeErr := out.Close()
		in.Close()
		if copyErr != nil {
			return res, copyErr
		}
		if closeErr != nil {
			return res, closeErr
		}
		res.ArtifactRefs = append(res.ArtifactRefs, toolcmd.ArtifactRef{URI: "bashy://" + host + "/files-offers/" + runID + "/" + url.PathEscape(name), Size: size, SHA256: hex.EncodeToString(hash.Sum(nil))})
	}
	return res, runErr
}
