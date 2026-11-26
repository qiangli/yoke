// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package artifact

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"golang.org/x/mod/modfile"
	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"
)

const (
	// WorkflowHelperKind is the ManifestV1 RootDescriptor kind for staged local
	// workflow actions, runner helper scripts/binaries, and build locks.
	WorkflowHelperKind = "workflow-helper"

	// MediaTypeWorkflowHelper is the CAS media type for workflow-helper archives.
	MediaTypeWorkflowHelper = "application/vnd.bashy.workflow-helper.v1+tar"

	// WorkflowHelperSchemaV1 identifies the leading metadata manifest inside a
	// workflow-helper archive.
	WorkflowHelperSchemaV1 = "bashy-workflow-helper-v1"

	// FixedPointSchemaV1 identifies FixedPointReportV1 JSON output.
	FixedPointSchemaV1 = "bashy-selfhost-fixedpoint-v1"

	workflowHelperMetadataMember = "workflow-helper-metadata.json"
	logicalPrefixWorkflowHelper  = "workflow-helper/"
	goModProxyURLEnv             = "BASHY_GOMOD_PROXY_URL"
	loomRepoEnv                  = "BASHY_LOOM_DIR"
)

// ErrFixedPointMismatch is returned when Stage 1 (candidate_1) and Stage 2
// (candidate_2) outputs of RunSelfHostFixedPoint are not byte-identical.
var ErrFixedPointMismatch = errors.New("artifact: self-hosting fixed-point mismatch between candidate_1 and candidate_2")

// WorkflowHelperFileEntry pins the archive-relative path, size, SHA-256 hex, and
// executable permission bit of one staged workflow helper file.
type WorkflowHelperFileEntry struct {
	Path       string `json:"path"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable bool   `json:"executable,omitempty"`
}

// WorkflowHelperMetadataV1 is serialized as workflow-helper-metadata.json at the
// front of every workflow-helper archive in CAS.
type WorkflowHelperMetadataV1 struct {
	SchemaVersion string                    `json:"schema_version"`
	Name          string                    `json:"name"`
	Version       string                    `json:"version"`
	Platform      string                    `json:"platform,omitempty"`
	SourceCommits map[string]string         `json:"source_commits,omitempty"`
	BuildImage    string                    `json:"build_image,omitempty"`
	BuildDigest   string                    `json:"build_image_digest,omitempty"`
	Files         []WorkflowHelperFileEntry `json:"files"`
}

// WorkflowHelperCaptureOptions configures CaptureWorkflowHelpers.
type WorkflowHelperCaptureOptions struct {
	Name          string
	Version       string
	Platform      string
	SourceDir     string
	ExtraPaths    map[string]string
	CommitsFile   string
	BuildLockPath string
}

// WorkflowHelperCaptureResult reports the protected CAS root and metadata of a
// captured workflow-helper bundle.
type WorkflowHelperCaptureResult struct {
	Root       RootDescriptor           `json:"root"`
	Digest     string                   `json:"digest"`
	FileCount  int                      `json:"file_count"`
	TotalBytes int64                    `json:"total_bytes"`
	Metadata   WorkflowHelperMetadataV1 `json:"metadata"`
}

// WorkflowHelperMaterializeOptions configures MaterializeWorkflowHelpers.
type WorkflowHelperMaterializeOptions struct {
	Name    string
	Version string
	Digest  string
	DestDir string
}

// WorkflowHelperMaterializeResult summarizes verified local workflow-helper extraction.
type WorkflowHelperMaterializeResult struct {
	Name            string                   `json:"name"`
	Version         string                   `json:"version"`
	Digest          string                   `json:"digest"`
	DestDir         string                   `json:"dest_dir"`
	FileCount       int                      `json:"file_count"`
	TotalBytes      int64                    `json:"total_bytes"`
	Materialized    []string                 `json:"materialized"`
	VerifiedArchive bool                     `json:"verified_archive"`
	Metadata        WorkflowHelperMetadataV1 `json:"metadata"`
}

func parseCommitsManifestFile(filePath string) (map[string]string, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := make(map[string]string)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSuffix(strings.TrimSpace(k), "_commit")
		v = strings.TrimSpace(v)
		if k != "" && v != "" {
			out[k] = v
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type stagedHelperMember struct {
	archivePath string
	diskPath    string
	size        int64
	sha256Hex   string
	executable  bool
}

func collectWorkflowHelperMembers(prefix, srcPath string, seen map[string]bool) ([]stagedHelperMember, error) {
	fi, err := os.Stat(srcPath)
	if err != nil {
		return nil, err
	}
	addOne := func(archPath, dPath string, info fs.FileInfo) (stagedHelperMember, error) {
		if !SafeLogicalPath(archPath) || archPath == workflowHelperMetadataMember {
			return stagedHelperMember{}, fmt.Errorf("artifact: invalid workflow helper path %q", archPath)
		}
		if seen[archPath] {
			return stagedHelperMember{}, fmt.Errorf("artifact: duplicate workflow helper archive path %q", archPath)
		}
		sumHex, sz, err := fileSHA256AndSize(dPath)
		if err != nil {
			return stagedHelperMember{}, err
		}
		if sz != info.Size() {
			return stagedHelperMember{}, fmt.Errorf("artifact: file size changed while staging %s", dPath)
		}
		seen[archPath] = true
		return stagedHelperMember{
			archivePath: archPath,
			diskPath:    dPath,
			size:        sz,
			sha256Hex:   sumHex,
			executable:  info.Mode()&0o111 != 0,
		}, nil
	}

	if fi.Mode().IsRegular() {
		archName := strings.Trim(prefix, "/")
		if archName == "" {
			archName = filepath.Base(srcPath)
		}
		m, err := addOne(archName, srcPath, fi)
		if err != nil {
			return nil, err
		}
		return []stagedHelperMember{m}, nil
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("artifact: workflow helper source %s must be a regular file or directory", srcPath)
	}

	var out []stagedHelperMember
	err = filepath.WalkDir(srcPath, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(srcPath, p)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == workflowHelperMetadataMember {
			return nil
		}
		archName := relSlash
		if trimmedPrefix := strings.Trim(prefix, "/"); trimmedPrefix != "" {
			archName = path.Join(trimmedPrefix, relSlash)
		}
		m, err := addOne(archName, p, info)
		if err != nil {
			return err
		}
		out = append(out, m)
		return nil
	})
	return out, err
}

// CaptureWorkflowHelpers packages local workflow actions, runner helper scripts
// or binaries, build image locks, and exact commit manifests into CAS as a
// RetentionProtectedRoot (Kind: "workflow-helper", MediaType:
// "application/vnd.bashy.workflow-helper.v1+tar").
func CaptureWorkflowHelpers(ctx context.Context, backend Backend, opts WorkflowHelperCaptureOptions) (*WorkflowHelperCaptureResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}
	if strings.TrimSpace(opts.SourceDir) == "" && len(opts.ExtraPaths) == 0 {
		return nil, errors.New("artifact: CaptureWorkflowHelpers requires SourceDir or ExtraPaths")
	}

	rawName := strings.TrimSpace(opts.Name)
	rawName = strings.TrimPrefix(rawName, logicalPrefixWorkflowHelper)
	if rawName == "" && strings.TrimSpace(opts.SourceDir) != "" {
		rawName = filepath.Base(filepath.Clean(opts.SourceDir))
	}
	cleanName := normalizeOCILogicalRepo(rawName)
	if cleanName == "" || !SafeLogicalPath(cleanName) {
		return nil, fmt.Errorf("artifact: invalid workflow helper name %q", opts.Name)
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" {
		version = "v1"
	}
	platform := strings.TrimSpace(opts.Platform)
	if platform == "" {
		platform = CurrentPlatform()
	}

	seen := make(map[string]bool)
	var members []stagedHelperMember

	if src := strings.TrimSpace(opts.SourceDir); src != "" {
		sub, err := collectWorkflowHelperMembers("", src, seen)
		if err != nil {
			return nil, err
		}
		members = append(members, sub...)
	}

	if len(opts.ExtraPaths) > 0 {
		keys := make([]string, 0, len(opts.ExtraPaths))
		for k := range opts.ExtraPaths {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		for _, k := range keys {
			cleanKey := normalizeOCILogicalRepo(k)
			sub, err := collectWorkflowHelperMembers(cleanKey, opts.ExtraPaths[k], seen)
			if err != nil {
				return nil, fmt.Errorf("artifact: capture workflow helper %q: %w", k, err)
			}
			members = append(members, sub...)
		}
	}

	commitsMap := make(map[string]string)
	commitsPath := strings.TrimSpace(opts.CommitsFile)
	if commitsPath == "" && strings.TrimSpace(opts.SourceDir) != "" {
		cand := filepath.Join(opts.SourceDir, "commits.txt")
		if st, err := os.Stat(cand); err == nil && st.Mode().IsRegular() {
			commitsPath = cand
		}
	}
	if commitsPath != "" {
		parsedCommits, err := parseCommitsManifestFile(commitsPath)
		if err != nil {
			return nil, fmt.Errorf("artifact: parse commits manifest %s: %w", commitsPath, err)
		}
		for k, v := range parsedCommits {
			commitsMap[k] = v
		}
		if !seen["commits.txt"] {
			sub, err := collectWorkflowHelperMembers("commits.txt", commitsPath, seen)
			if err != nil {
				return nil, err
			}
			members = append(members, sub...)
		}
	}

	var buildImage, buildDigest string
	lockPath := strings.TrimSpace(opts.BuildLockPath)
	if lockPath == "" && strings.TrimSpace(opts.SourceDir) != "" {
		for _, relCand := range []string{"image.lock", filepath.FromSlash("tools/bashy-build/image.lock")} {
			cand := filepath.Join(opts.SourceDir, relCand)
			if st, err := os.Stat(cand); err == nil && st.Mode().IsRegular() {
				lockPath = cand
				break
			}
		}
	}
	if lockPath != "" {
		lock, err := ParseBuildImageLock(lockPath)
		if err != nil {
			return nil, fmt.Errorf("artifact: parse build lock %s: %w", lockPath, err)
		}
		buildImage = lock.Image
		buildDigest = lock.ImageDigest
		for k, v := range lock.SourceCommits {
			if _, exists := commitsMap[k]; !exists {
				commitsMap[k] = v
			}
		}
		if !seen["image.lock"] && !seen["tools/bashy-build/image.lock"] {
			sub, err := collectWorkflowHelperMembers("image.lock", lockPath, seen)
			if err != nil {
				return nil, err
			}
			members = append(members, sub...)
		}
	}

	if len(members) == 0 {
		return nil, errors.New("artifact: workflow helper capture found zero regular files")
	}

	slices.SortFunc(members, func(a, b stagedHelperMember) int {
		return strings.Compare(a.archivePath, b.archivePath)
	})

	fileEntries := make([]WorkflowHelperFileEntry, 0, len(members))
	var totalBytes int64
	for _, m := range members {
		fileEntries = append(fileEntries, WorkflowHelperFileEntry{
			Path:       m.archivePath,
			Size:       m.size,
			SHA256:     m.sha256Hex,
			Executable: m.executable,
		})
		totalBytes += m.size
	}
	if len(commitsMap) == 0 {
		commitsMap = nil
	}

	meta := WorkflowHelperMetadataV1{
		SchemaVersion: WorkflowHelperSchemaV1,
		Name:          cleanName,
		Version:       version,
		Platform:      platform,
		SourceCommits: commitsMap,
		BuildImage:    buildImage,
		BuildDigest:   buildDigest,
		Files:         fileEntries,
	}
	metaBytes, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return nil, err
	}
	metaBytes = append(metaBytes, '\n')

	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	fixedTime := time.Unix(0, 0).UTC()

	if err := tw.WriteHeader(&tar.Header{
		Name:     workflowHelperMetadataMember,
		Typeflag: tar.TypeReg,
		Mode:     0o644,
		Size:     int64(len(metaBytes)),
		ModTime:  fixedTime,
	}); err != nil {
		return nil, err
	}
	if _, err := tw.Write(metaBytes); err != nil {
		return nil, err
	}

	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		mode := int64(0o644)
		if m.executable {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{
			Name:     m.archivePath,
			Typeflag: tar.TypeReg,
			Mode:     mode,
			Size:     m.size,
			ModTime:  fixedTime,
		}); err != nil {
			return nil, err
		}
		in, err := os.Open(m.diskPath)
		if err != nil {
			return nil, err
		}
		fh := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tw, fh), in)
		closeErr := in.Close()
		if copyErr != nil {
			return nil, copyErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if n != m.size || hex.EncodeToString(fh.Sum(nil)) != m.sha256Hex {
			return nil, fmt.Errorf("artifact: helper file %s mutated during capture", m.archivePath)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}

	tarBytes := tarBuf.Bytes()
	dgst := DigestBytes(tarBytes)
	logicalName := logicalPrefixWorkflowHelper + cleanName
	provenance := "workflow-helper:" + cleanName + "@" + version
	if buildDigest != "" {
		provenance += ";image=" + buildDigest
	}

	if _, err := backend.PutArtifact(ctx, CatalogRecordV1{
		SchemaVersion:  CatalogRecordSchemaV1,
		Digest:         dgst,
		Size:           int64(len(tarBytes)),
		MediaType:      MediaTypeWorkflowHelper,
		Platform:       platform,
		LogicalName:    logicalName,
		LogicalVersion: version,
		Provenance:     provenance,
		Retention:      RetentionProtectedRoot,
		RetentionRoots: []string{"workflow-helper:" + cleanName + "@" + dgst},
	}, bytes.NewReader(tarBytes)); err != nil {
		return nil, err
	}

	root := RootDescriptor{
		Kind:      WorkflowHelperKind,
		Name:      logicalName,
		Version:   version,
		Platform:  platform,
		MediaType: MediaTypeWorkflowHelper,
		Digest:    dgst,
		Size:      int64(len(tarBytes)),
	}
	if err := root.Validate(); err != nil {
		return nil, err
	}

	return &WorkflowHelperCaptureResult{
		Root:       root,
		Digest:     dgst,
		FileCount:  len(fileEntries),
		TotalBytes: totalBytes,
		Metadata:   meta,
	}, nil
}

func withOptionalReadLease(ctx context.Context, backend Backend, ttl time.Duration, digests []string, fn func(context.Context) error) error {
	if st, ok := backend.(*Store); ok && st.readOnly {
		return fn(ctx)
	}
	return backend.WithReadLease(ctx, 0, "", ttl, digests, fn)
}

func resolveWorkflowHelperRecord(ctx context.Context, backend Backend, opts WorkflowHelperMaterializeOptions) (string, string, string, error) {
	explicitDigest := strings.TrimSpace(opts.Digest)
	rawName := strings.TrimSpace(opts.Name)
	if ValidDigest(rawName) && explicitDigest == "" {
		explicitDigest = rawName
		rawName = ""
	}
	if explicitDigest != "" && !ValidDigest(explicitDigest) {
		return "", "", "", fmt.Errorf("artifact: invalid workflow helper digest %q", explicitDigest)
	}

	if rawName == "" {
		if explicitDigest == "" {
			return "", "", "", errors.New("artifact: MaterializeWorkflowHelpers requires Name or Digest")
		}
		st, err := backend.GetRecordStatus(ctx, explicitDigest)
		if err != nil {
			return "", "", "", err
		}
		if !st.PayloadPresent {
			return "", "", "", fmt.Errorf("%w: %s", ErrPayloadEvicted, explicitDigest)
		}
		return st.Record.Digest, strings.TrimPrefix(st.Record.LogicalName, logicalPrefixWorkflowHelper), st.Record.LogicalVersion, nil
	}

	cleanName := normalizeOCILogicalRepo(strings.TrimPrefix(rawName, logicalPrefixWorkflowHelper))
	logicalName := logicalPrefixWorkflowHelper + cleanName
	ver := strings.TrimSpace(opts.Version)

	if ver != "" {
		st, err := backend.LookupLogical(ctx, logicalName, ver)
		if err != nil {
			return "", "", "", err
		}
		if !st.PayloadPresent {
			return "", "", "", fmt.Errorf("%w: %s", ErrPayloadEvicted, st.Record.Digest)
		}
		if explicitDigest != "" && st.Record.Digest != explicitDigest {
			return "", "", "", fmt.Errorf("%w: workflow helper %s@%s digest %s != %s",
				ErrCorruptPayload, logicalName, ver, st.Record.Digest, explicitDigest)
		}
		return st.Record.Digest, cleanName, st.Record.LogicalVersion, nil
	}

	if st, err := backend.LookupLogical(ctx, logicalName, "v1"); err == nil && st.PayloadPresent {
		if explicitDigest != "" && st.Record.Digest != explicitDigest {
			return "", "", "", fmt.Errorf("%w: workflow helper %s@v1 digest %s != %s",
				ErrCorruptPayload, logicalName, st.Record.Digest, explicitDigest)
		}
		return st.Record.Digest, cleanName, st.Record.LogicalVersion, nil
	}

	if rl, ok := backend.(interface {
		ListRecords(context.Context) ([]RecordStatus, error)
	}); ok {
		if recs, err := rl.ListRecords(ctx); err == nil {
			var best *RecordStatus
			for i := range recs {
				rs := &recs[i]
				if rs.Record.LogicalName == logicalName && rs.PayloadPresent {
					if best == nil || rs.Record.CreatedAt.After(best.Record.CreatedAt) {
						best = rs
					}
				}
			}
			if best != nil {
				if explicitDigest != "" && best.Record.Digest != explicitDigest {
					return "", "", "", fmt.Errorf("%w: workflow helper %s digest %s != %s",
						ErrCorruptPayload, logicalName, best.Record.Digest, explicitDigest)
				}
				return best.Record.Digest, cleanName, best.Record.LogicalVersion, nil
			}
		}
	}

	return "", "", "", fmt.Errorf("%w: workflow helper %q", ErrNotFound, logicalName)
}

// MaterializeWorkflowHelpers streams a protected workflow-helper archive from
// CAS under a read lease, cryptographically verifies both the outer archive
// digest and every individual file's SHA-256 in a staging directory, and
// materializes the verified files into opts.DestDir with executable bits preserved
// and zero network access.
func MaterializeWorkflowHelpers(ctx context.Context, backend Backend, opts WorkflowHelperMaterializeOptions) (*WorkflowHelperMaterializeResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if backend == nil {
		return nil, errors.New("artifact: nil backend")
	}
	if strings.TrimSpace(opts.DestDir) == "" {
		return nil, errors.New("artifact: MaterializeWorkflowHelpers requires DestDir")
	}
	destAbs, err := filepath.Abs(opts.DestDir)
	if err != nil {
		return nil, err
	}

	dgst, resolvedName, resolvedVer, err := resolveWorkflowHelperRecord(ctx, backend, opts)
	if err != nil {
		return nil, err
	}

	var result *WorkflowHelperMaterializeResult
	err = withOptionalReadLease(ctx, backend, defaultExportLeaseTTL, []string{dgst}, func(leaseCtx context.Context) error {
		rc, err := backend.OpenBlob(leaseCtx, dgst)
		if err != nil {
			return err
		}
		defer rc.Close()

		if err := os.MkdirAll(filepath.Dir(destAbs), 0o755); err != nil {
			return err
		}
		stageDir, err := os.MkdirTemp(filepath.Dir(destAbs), ".workflow-helper-stage-*")
		if err != nil {
			return err
		}
		defer os.RemoveAll(stageDir)

		outerHasher := sha256.New()
		tee := io.TeeReader(rc, outerHasher)
		tr := tar.NewReader(tee)

		firstHdr, err := tr.Next()
		if err != nil {
			return fmt.Errorf("%w: read workflow-helper header: %v", ErrCorruptPayload, err)
		}
		if firstHdr.Name != workflowHelperMetadataMember || (firstHdr.Typeflag != tar.TypeReg && firstHdr.Typeflag != tar.TypeRegA) {
			return fmt.Errorf("%w: workflow-helper archive must start with %s (got %q)",
				ErrCorruptPayload, workflowHelperMetadataMember, firstHdr.Name)
		}
		if firstHdr.Size <= 0 || firstHdr.Size > maxOCIMetadataBytes {
			return fmt.Errorf("%w: invalid %s size %d", ErrCorruptPayload, workflowHelperMetadataMember, firstHdr.Size)
		}
		metaBytes, err := io.ReadAll(io.LimitReader(tr, firstHdr.Size+1))
		if err != nil {
			return err
		}
		if int64(len(metaBytes)) != firstHdr.Size {
			return fmt.Errorf("%w: truncated %s", ErrCorruptPayload, workflowHelperMetadataMember)
		}

		var meta WorkflowHelperMetadataV1
		if err := json.Unmarshal(metaBytes, &meta); err != nil {
			return fmt.Errorf("%w: decode %s: %v", ErrCorruptPayload, workflowHelperMetadataMember, err)
		}
		if meta.SchemaVersion != WorkflowHelperSchemaV1 {
			return fmt.Errorf("%w: unsupported workflow helper schema %q", ErrCorruptPayload, meta.SchemaVersion)
		}

		expected := make(map[string]WorkflowHelperFileEntry, len(meta.Files))
		for _, fe := range meta.Files {
			if !SafeLogicalPath(fe.Path) || !hex64RE.MatchString(fe.SHA256) || fe.Size < 0 {
				return fmt.Errorf("%w: invalid file entry %q in %s", ErrCorruptPayload, fe.Path, workflowHelperMetadataMember)
			}
			expected[fe.Path] = fe
		}

		seenFiles := make(map[string]bool, len(meta.Files))
		cleanStage := filepath.Clean(stageDir)
		var totalBytes int64

		for {
			if err := leaseCtx.Err(); err != nil {
				return err
			}
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("%w: read workflow-helper tar stream: %v", ErrCorruptPayload, err)
			}
			if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
				return fmt.Errorf("%w: unsupported tar entry type %d (%q) in workflow-helper archive",
					ErrCorruptPayload, hdr.Typeflag, hdr.Name)
			}
			if !SafeLogicalPath(hdr.Name) {
				return fmt.Errorf("%w: unsafe workflow-helper entry %q", ErrCorruptPayload, hdr.Name)
			}
			fe, ok := expected[hdr.Name]
			if !ok {
				return fmt.Errorf("%w: unexpected entry %q in workflow-helper archive", ErrCorruptPayload, hdr.Name)
			}
			if seenFiles[hdr.Name] {
				return fmt.Errorf("%w: duplicate entry %q in workflow-helper archive", ErrCorruptPayload, hdr.Name)
			}
			if hdr.Size != fe.Size {
				return fmt.Errorf("%w: entry %q size %d != manifest %d", ErrCorruptPayload, hdr.Name, hdr.Size, fe.Size)
			}

			stagePath := filepath.Join(cleanStage, filepath.FromSlash(hdr.Name))
			if stagePath != cleanStage && !strings.HasPrefix(stagePath, cleanStage+string(filepath.Separator)) {
				return fmt.Errorf("%w: entry %q escapes staging directory", ErrCorruptPayload, hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(stagePath), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if fe.Executable {
				mode = 0o755
			}
			outF, err := os.OpenFile(stagePath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
			if err != nil {
				return err
			}
			fh := sha256.New()
			n, copyErr := io.Copy(io.MultiWriter(outF, fh), io.LimitReader(tr, fe.Size+1))
			closeErr := outF.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
			if n != fe.Size || hex.EncodeToString(fh.Sum(nil)) != fe.SHA256 {
				return fmt.Errorf("%w: entry %q sha256 mismatch during streaming verification", ErrCorruptPayload, hdr.Name)
			}
			seenFiles[hdr.Name] = true
			totalBytes += n
		}

		// Drain trailing tar zero-padding blocks and verify full outer CAS blob digest!
		if _, err := io.Copy(io.Discard, tee); err != nil {
			return fmt.Errorf("%w: drain workflow-helper stream: %v", ErrCorruptPayload, err)
		}
		if gotDigest := "sha256:" + hex.EncodeToString(outerHasher.Sum(nil)); gotDigest != dgst {
			return fmt.Errorf("%w: workflow-helper outer digest %s != %s", ErrCorruptPayload, gotDigest, dgst)
		}
		if len(seenFiles) != len(expected) {
			return fmt.Errorf("%w: workflow-helper archive missing %d expected file(s)",
				ErrCorruptPayload, len(expected)-len(seenFiles))
		}

		// Commit verified files from stageDir into destAbs.
		if err := os.MkdirAll(destAbs, 0o755); err != nil {
			return err
		}
		cleanDest := filepath.Clean(destAbs)
		materialized := make([]string, 0, len(meta.Files))
		for _, fe := range meta.Files {
			srcPath := filepath.Join(cleanStage, filepath.FromSlash(fe.Path))
			dstPath := filepath.Join(cleanDest, filepath.FromSlash(fe.Path))
			if dstPath != cleanDest && !strings.HasPrefix(dstPath, cleanDest+string(filepath.Separator)) {
				return fmt.Errorf("artifact: destination path escapes root: %q", fe.Path)
			}
			if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if fe.Executable {
				mode = 0o755
			}
			if err := copyVerifiedFileAtomic(srcPath, dstPath, mode); err != nil {
				return err
			}
			materialized = append(materialized, dstPath)
		}

		if resolvedName == "" {
			resolvedName = meta.Name
		}
		if resolvedVer == "" {
			resolvedVer = meta.Version
		}
		result = &WorkflowHelperMaterializeResult{
			Name:            resolvedName,
			Version:         resolvedVer,
			Digest:          dgst,
			DestDir:         destAbs,
			FileCount:       len(materialized),
			TotalBytes:      totalBytes,
			Materialized:    materialized,
			VerifiedArchive: true,
			Metadata:        meta,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func copyVerifiedFileAtomic(srcPath, dstPath string, mode os.FileMode) error {
	in, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp, err := os.CreateTemp(filepath.Dir(dstPath), "."+filepath.Base(dstPath)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dstPath); err != nil {
		return err
	}
	cleanup = false
	return os.Chmod(dstPath, mode)
}

// FixedPointOptions configures RunSelfHostFixedPoint.
type FixedPointOptions struct {
	PrevBashyExe          string
	SourceDir             string
	TargetPackage         string
	ArtifactRoot          string
	LoomDir               string
	Backend               Backend
	Version               string
	BuildID               string
	BuildLockPath         string
	UseProductOverlay     bool
	DisableProductOverlay bool
	GoVersion             string
	ProxyURL              string
	StartProxy            func(ctx context.Context, artifactRoot, loomDir string) (string, func() error, error)
	SmokeArgs             []string
	OutputBinary          string
	ExtraLdflags          string
}

// FixedPointStageReport records inputs, digests, timing, and disposable cache
// footprint for one stage of the 3-stage offline self-hosting fixed-point gate.
type FixedPointStageReport struct {
	Stage          int    `json:"stage"`
	Name           string `json:"name"`
	DriverExe      string `json:"driver_exe"`
	DriverSHA256   string `json:"driver_sha256"`
	OutputBinary   string `json:"output_binary"`
	OutputSHA256   string `json:"output_sha256"`
	OutputSize     int64  `json:"output_size"`
	DurationMillis int64  `json:"duration_ms"`
	WorkspaceBytes uint64 `json:"workspace_bytes"`
	GoCacheEmpty   bool   `json:"gocache_started_empty"`
	ModCacheEmpty  bool   `json:"gomodcache_started_empty"`
}

// FixedPointReportV1 is the structured evidence envelope returned by
// RunSelfHostFixedPoint (schema_version: "bashy-selfhost-fixedpoint-v1").
type FixedPointReportV1 struct {
	SchemaVersion      string                  `json:"schema_version"`
	Platform           string                  `json:"platform"`
	SourceDir          string                  `json:"source_dir"`
	TargetPackage      string                  `json:"target_package"`
	Version            string                  `json:"version"`
	BuildID            string                  `json:"build_id"`
	ArtifactRoot       string                  `json:"artifact_root"`
	LoomDir            string                  `json:"loom_dir"`
	Sealed             bool                    `json:"sealed"`
	ProductOverlayUsed bool                    `json:"product_overlay_used"`
	ProxyURL           string                  `json:"proxy_url,omitempty"`
	BuildLock          *BuildImageLock         `json:"build_lock,omitempty"`
	Stages             []FixedPointStageReport `json:"stages"`
	Candidate0SHA256   string                  `json:"candidate_0_sha256"`
	Candidate0Size     int64                   `json:"candidate_0_size"`
	Candidate1SHA256   string                  `json:"candidate_1_sha256"`
	Candidate1Size     int64                   `json:"candidate_1_size"`
	Candidate2SHA256   string                  `json:"candidate_2_sha256"`
	Candidate2Size     int64                   `json:"candidate_2_size"`
	FixedPointMatch    bool                    `json:"fixed_point_match"`
	SmokeArgs          []string                `json:"smoke_args,omitempty"`
	SmokeOutput        string                  `json:"smoke_output,omitempty"`
	TotalDurationMS    int64                   `json:"total_duration_ms"`
	PeakWorkspaceBytes uint64                  `json:"peak_workspace_bytes"`
	OutputBinary       string                  `json:"output_binary,omitempty"`
}

func candidateBinName() string {
	if runtime.GOOS == "windows" {
		return "bashy.exe"
	}
	return "bashy"
}

func isLoopbackHTTPProxyURL(raw string) bool {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || !strings.EqualFold(u.Scheme, "http") || u.Host == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return ip != nil && ip.IsLoopback()
}

func resolveDeterministicBuildID(srcDir string, lock *BuildImageLock, explicit string) string {
	if id := strings.TrimSpace(explicit); id != "" {
		return id
	}
	if lock != nil {
		if sha := strings.TrimSpace(lock.SourceCommits["bashy"]); len(sha) >= 7 {
			return sha[:7]
		}
	}
	for _, cand := range []string{
		filepath.Join(srcDir, "commits.txt"),
		filepath.Join(filepath.Dir(srcDir), "commits.txt"),
	} {
		if m, err := parseCommitsManifestFile(cand); err == nil {
			if sha := strings.TrimSpace(m["bashy"]); len(sha) >= 7 {
				return sha[:7]
			}
		}
	}
	if repo, err := gogit.PlainOpenWithOptions(srcDir, &gogit.PlainOpenOptions{DetectDotGit: true}); err == nil {
		if head, hErr := repo.Head(); hErr == nil {
			s := head.Hash().String()
			if len(s) >= 7 {
				return s[:7]
			}
		}
	}
	return "fixedpt"
}

func readShellRuntimeStampFromGoMod(goModPath string) string {
	raw, err := os.ReadFile(goModPath)
	if err != nil {
		return ""
	}
	mf, err := modfile.ParseLax(goModPath, raw, nil)
	if err != nil {
		return ""
	}
	for _, r := range mf.Replace {
		if r.Old.Path == "mvdan.cc/sh/v3" && r.New.Path == "github.com/qiangli/sh/v3" {
			return r.New.Version
		}
	}
	return ""
}

func writeFailingMakeStubs(stubDir string) error {
	if err := os.MkdirAll(stubDir, 0o755); err != nil {
		return err
	}
	posixStub := []byte("#!/bin/sh\necho \"bashy self-host fixed-point must not call make\" >&2\nexit 127\n")
	for _, name := range []string{"make", "gmake"} {
		p := filepath.Join(stubDir, name)
		if err := os.WriteFile(p, posixStub, 0o755); err != nil {
			return err
		}
	}
	if runtime.GOOS == "windows" {
		winStub := []byte("@echo bashy self-host fixed-point must not call make 1>&2\r\n@exit /b 127\r\n")
		for _, name := range []string{"make.bat", "make.cmd", "gmake.bat", "gmake.cmd"} {
			p := filepath.Join(stubDir, name)
			if err := os.WriteFile(p, winStub, 0o755); err != nil {
				return err
			}
		}
	}
	return nil
}

func dirSizeBytes(root string) uint64 {
	var total uint64
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			if info.Size() > 0 {
				total += uint64(info.Size())
			}
		}
		return nil
	})
	return total
}

func isEmptyDir(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) == 0
}

func removeWritableTree(dir string) {
	_ = filepath.WalkDir(dir, func(p string, _ fs.DirEntry, _ error) error {
		_ = os.Chmod(p, 0o700)
		return nil
	})
	_ = os.RemoveAll(dir)
}

func filesByteIdentical(pathA, pathB string) (bool, error) {
	fa, err := os.Open(pathA)
	if err != nil {
		return false, err
	}
	defer fa.Close()
	fb, err := os.Open(pathB)
	if err != nil {
		return false, err
	}
	defer fb.Close()

	ia, err := fa.Stat()
	if err != nil {
		return false, err
	}
	ib, err := fb.Stat()
	if err != nil {
		return false, err
	}
	if ia.Size() != ib.Size() {
		return false, nil
	}

	bufA := make([]byte, 64<<10)
	bufB := make([]byte, 64<<10)
	for {
		nA, errA := io.ReadFull(fa, bufA)
		nB, errB := io.ReadFull(fb, bufB)
		if nA != nB || !bytes.Equal(bufA[:nA], bufB[:nB]) {
			return false, nil
		}
		if errA == io.EOF && errB == io.EOF {
			return true, nil
		}
		if errors.Is(errA, io.ErrUnexpectedEOF) && errors.Is(errB, io.ErrUnexpectedEOF) {
			return true, nil
		}
		if errA != nil || errB != nil {
			return false, errors.Join(errA, errB)
		}
	}
}

func buildHermeticStageEnv(baseEnv []string, overrides map[string]string, drop map[string]bool) []string {
	out := make([]string, 0, len(baseEnv)+len(overrides))
	for _, kv := range baseEnv {
		k, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if drop[k] {
			continue
		}
		if _, replaced := overrides[k]; replaced {
			continue
		}
		out = append(out, kv)
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// sealedArtifactGoProxy serves standard GOPROXY HTTP endpoints strictly from
// local Backend CAS + durable Loom refs (including single-flight .zip
// reconstruction when derived module zips were evicted), never accessing the network.
type sealedArtifactGoProxy struct {
	backend Backend
	loomDir string
	mu      sync.Mutex
}

func (p *sealedArtifactGoProxy) fetchBlobBytes(ctx context.Context, dgst string) ([]byte, error) {
	var out []byte
	err := withOptionalReadLease(ctx, p.backend, 2*time.Minute, []string{dgst}, func(lctx context.Context) error {
		var fErr error
		out, fErr = p.backend.FetchVerified(lctx, dgst)
		return fErr
	})
	return out, err
}

func (p *sealedArtifactGoProxy) listVersions(ctx context.Context, modPath string) ([]string, error) {
	pubs, err := p.backend.ListPublications(ctx)
	if err != nil {
		return nil, err
	}
	var vers []string
	for _, pub := range pubs {
		if pub.Module == modPath {
			vers = append(vers, pub.Version)
		}
	}
	if st, lErr := p.backend.LookupLogical(ctx, "gomod-cached-versions/"+modPath, "list"); lErr == nil && st.PayloadPresent {
		if raw, rErr := p.fetchBlobBytes(ctx, st.Record.Digest); rErr == nil {
			vers = append(vers, strings.Fields(string(raw))...)
		}
	}
	semver.Sort(vers)
	return slices.Compact(vers), nil
}

func (p *sealedArtifactGoProxy) servePublishedOrCachedZip(ctx context.Context, modPath, ver string) ([]byte, int, error) {
	if pub, pubErr := p.backend.GetPublication(ctx, modPath, ver); pubErr == nil {
		if st, stErr := p.backend.GetRecordStatus(ctx, pub.ZipDigest); stErr == nil && st.PayloadPresent {
			if data, rErr := p.fetchBlobBytes(ctx, pub.ZipDigest); rErr == nil {
				return data, http.StatusOK, nil
			}
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if st, stErr := p.backend.GetRecordStatus(ctx, pub.ZipDigest); stErr == nil && st.PayloadPresent {
			if data, rErr := p.fetchBlobBytes(ctx, pub.ZipDigest); rErr == nil {
				return data, http.StatusOK, nil
			}
		}
		repo, err := gogit.PlainOpen(p.loomDir)
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("%w: open loom %s: %v", ErrMissingRecoveryInputs, p.loomDir, err)
		}
		zipBytes, err := reconstructZipFromLoomRepo(repo, pub)
		if err != nil {
			return nil, http.StatusInternalServerError, fmt.Errorf("%w: %v", ErrMissingRecoveryInputs, err)
		}
		// Best-effort restore into CAS when backend is writable.
		if store, isStore := p.backend.(*Store); !isStore || !store.readOnly {
			var inputDigests []string
			if st, err := p.backend.GetRecordStatus(ctx, pub.ZipDigest); err == nil && st.Record.Recipe != nil {
				inputDigests = slices.Clone(st.Record.Recipe.InputDigests)
			}
			_, _ = p.backend.PutArtifact(ctx, CatalogRecordV1{
				SchemaVersion:  CatalogRecordSchemaV1,
				Digest:         pub.ZipDigest,
				Size:           int64(len(zipBytes)),
				MediaType:      "application/zip",
				LogicalName:    "gomod-zip/" + pub.Module,
				LogicalVersion: pub.Version,
				Provenance:     "loom:" + pub.LoomRef,
				Retention:      RetentionReconstructible,
				Recipe: &RegenerationRecipe{
					Kind:           RecipeGoModZipV1,
					InputDigests:   inputDigests,
					LoomRef:        pub.LoomRef,
					Commit:         pub.Commit,
					Subdir:         pub.Subdir,
					ExpectedDigest: pub.ZipDigest,
					ExpectedGoSum:  pub.GoSumH1,
				},
			}, bytes.NewReader(zipBytes))
		}
		return zipBytes, http.StatusOK, nil
	}

	st, err := p.backend.LookupLogical(ctx, "gomod-zip/"+modPath, ver)
	if err != nil || !st.PayloadPresent {
		return nil, http.StatusNotFound, ErrNotFound
	}
	data, err := p.fetchBlobBytes(ctx, st.Record.Digest)
	if err != nil {
		return nil, http.StatusInternalServerError, err
	}
	return data, http.StatusOK, nil
}

func (p *sealedArtifactGoProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	reqPath := strings.TrimPrefix(r.URL.Path, "/")
	if reqPath == "" || strings.HasPrefix(reqPath, "sumdb/") {
		http.NotFound(w, r)
		return
	}

	if escMod, isLatest := strings.CutSuffix(reqPath, "/@latest"); isLatest {
		modPath, err := module.UnescapePath(escMod)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		vers, err := p.listVersions(ctx, modPath)
		if err != nil || len(vers) == 0 {
			http.NotFound(w, r)
			return
		}
		chosen := vers[len(vers)-1]
		for _, v := range vers {
			if semver.Prerelease(v) == "" {
				chosen = v
			}
		}
		p.serveInfoOrMod(ctx, w, r, modPath, chosen, ".info")
		return
	}

	escMod, after, ok := strings.Cut(reqPath, "/@v/")
	if !ok {
		http.NotFound(w, r)
		return
	}
	modPath, err := module.UnescapePath(escMod)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if after == "list" {
		vers, err := p.listVersions(ctx, modPath)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if len(vers) > 0 {
			_, _ = w.Write([]byte(strings.Join(vers, "\n") + "\n"))
		}
		return
	}

	ext := path.Ext(after)
	if ext != ".info" && ext != ".mod" && ext != ".zip" {
		http.NotFound(w, r)
		return
	}
	ver, err := module.UnescapeVersion(strings.TrimSuffix(after, ext))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if ext == ".zip" {
		data, code, zErr := p.servePublishedOrCachedZip(ctx, modPath, ver)
		if zErr != nil {
			if code == http.StatusNotFound {
				http.NotFound(w, r)
			} else {
				http.Error(w, zErr.Error(), code)
			}
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(data)
		return
	}
	p.serveInfoOrMod(ctx, w, r, modPath, ver, ext)
}

func (p *sealedArtifactGoProxy) serveInfoOrMod(ctx context.Context, w http.ResponseWriter, r *http.Request, modPath, ver, ext string) {
	if pub, pubErr := p.backend.GetPublication(ctx, modPath, ver); pubErr == nil {
		dgst := pub.InfoDigest
		ct := "application/json"
		if ext == ".mod" {
			dgst = pub.ModDigest
			ct = "text/plain; charset=utf-8"
		}
		data, err := p.fetchBlobBytes(ctx, dgst)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write(data)
		return
	}
	prefix := "gomod-info/"
	ct := "application/json"
	if ext == ".mod" {
		prefix = "gomod-mod/"
		ct = "text/plain; charset=utf-8"
	}
	st, err := p.backend.LookupLogical(ctx, prefix+modPath, ver)
	if err != nil || !st.PayloadPresent {
		http.NotFound(w, r)
		return
	}
	data, err := p.fetchBlobBytes(ctx, st.Record.Digest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ct)
	_, _ = w.Write(data)
}

func startSealedLoopbackGoProxy(backend Backend, loomDir string) (string, func() error, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	httpSrv := &http.Server{
		Handler:           &sealedArtifactGoProxy{backend: backend, loomDir: loomDir},
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpSrv.Serve(ln)
	}()
	closeFn := func() error {
		cctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		err := httpSrv.Shutdown(cctx)
		<-done
		return err
	}
	return "http://" + ln.Addr().String(), closeFn, nil
}

// RunSelfHostFixedPoint executes the 3-stage offline self-hosting release gate:
//  1. Stage 0 (PrevBashyExe -> candidate_0) with fresh empty GOCACHE & GOMODCACHE.
//  2. Stage 1 (candidate_0 -> candidate_1) with fresh empty GOCACHE, GOMODCACHE, and BASHY_PRODUCTGO_CACHE.
//  3. Stage 2 (candidate_1 -> candidate_2) with fresh empty GOCACHE, GOMODCACHE, and BASHY_PRODUCTGO_CACHE.
//  4. Verifies sha256(candidate_1) == sha256(candidate_2) (and byte-by-byte equality),
//     runs a smoke command on candidate_2, and returns FixedPointReportV1.
func RunSelfHostFixedPoint(ctx context.Context, opts FixedPointOptions) (*FixedPointReportV1, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	overallStart := time.Now()

	prevExe := strings.TrimSpace(opts.PrevBashyExe)
	if prevExe == "" {
		exe, err := os.Executable()
		if err != nil || strings.TrimSpace(exe) == "" {
			return nil, errors.New("artifact: PrevBashyExe is required for RunSelfHostFixedPoint")
		}
		prevExe = exe
	}
	prevExeAbs, err := filepath.Abs(prevExe)
	if err != nil {
		return nil, err
	}
	prevStat, err := os.Stat(prevExeAbs)
	if err != nil || !prevStat.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact: PrevBashyExe %q is not a regular file: %v", prevExeAbs, err)
	}

	srcDir := strings.TrimSpace(opts.SourceDir)
	if srcDir == "" {
		srcDir = "."
	}
	srcAbs, err := filepath.Abs(srcDir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(filepath.Join(srcAbs, "go.mod")); err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("artifact: SourceDir %s has no go.mod", srcAbs)
	}

	targetPkg := strings.TrimSpace(opts.TargetPackage)
	if targetPkg == "" {
		targetPkg = "./cmd/bashy"
	}

	artRoot := resolveRoot(opts.ArtifactRoot)
	loomDir := strings.TrimSpace(opts.LoomDir)
	if loomDir == "" {
		loomDir = defaultLoomDirForRoot(artRoot)
	} else if abs, lErr := filepath.Abs(loomDir); lErr == nil {
		loomDir = abs
	}

	var buildLock *BuildImageLock
	if lp := strings.TrimSpace(opts.BuildLockPath); lp != "" {
		parsedLock, err := ParseBuildImageLock(lp)
		if err != nil {
			return nil, fmt.Errorf("artifact: parse BuildLockPath %s: %w", lp, err)
		}
		buildLock = parsedLock
	}

	version := strings.TrimSpace(opts.Version)
	if version == "" && buildLock != nil && buildLock.BashyVersion != "" {
		version = buildLock.BashyVersion
	}
	if version == "" {
		version = "dev"
	}
	buildID := resolveDeterministicBuildID(srcAbs, buildLock, opts.BuildID)

	productgoMain := filepath.Join(srcAbs, "tools", "productgo", "main.go")
	hasProductgo := false
	if st, err := os.Stat(productgoMain); err == nil && st.Mode().IsRegular() {
		hasProductgo = true
	}
	useProductOverlay := !opts.DisableProductOverlay && (opts.UseProductOverlay || (hasProductgo && os.Getenv("BASHY_SKIP_PRODUCTGO") != "1"))
	if useProductOverlay && !hasProductgo {
		return nil, fmt.Errorf("artifact: UseProductOverlay requested, but %s does not exist", productgoMain)
	}

	// Coordinate multi-process CAS access across PrevBashyExe, candidate_0, and candidate_1:
	// use an active loopback service if running, else start a temporary loopback Server when
	// writable, falling back to a read-only Store when artRoot is mounted read-only.
	backend := opts.Backend
	var cleanupBackend func()
	if backend == nil {
		if client, ok := TryConnectService(ctx, artRoot); ok {
			backend = client
			cleanupBackend = func() { _ = client.Close() }
		} else if fi, statErr := os.Stat(filepath.Join(artRoot, "catalog.db")); statErr == nil && !fi.IsDir() {
			if srv, srvErr := StartServer(ctx, ServerConfig{Root: artRoot, Addr: "127.0.0.1:0"}); srvErr == nil {
				backend = srv.Store()
				cleanupBackend = func() { _ = srv.Close() }
			} else {
				roStore, roErr := OpenStore(ctx, StoreOptions{Root: artRoot, ReadOnly: true})
				if roErr != nil {
					return nil, roErr
				}
				backend = roStore
				cleanupBackend = func() { _ = roStore.Close() }
			}
		}
	}
	if cleanupBackend != nil {
		defer cleanupBackend()
	}

	proxyURL := strings.TrimSpace(opts.ProxyURL)
	if proxyURL == "" {
		proxyURL = strings.TrimSpace(os.Getenv(goModProxyURLEnv))
	}
	var closeProxy func() error
	if proxyURL != "" {
		if proxyURL != "off" && !isLoopbackHTTPProxyURL(proxyURL) {
			return nil, fmt.Errorf("%w: non-loopback ProxyURL %q forbidden in offline fixed-point gate",
				ErrSealedNetworkDenied, proxyURL)
		}
	} else if opts.StartProxy != nil {
		startedURL, closeFn, err := opts.StartProxy(ctx, artRoot, loomDir)
		if err != nil {
			return nil, err
		}
		if startedURL != "" && startedURL != "off" && !isLoopbackHTTPProxyURL(startedURL) {
			if closeFn != nil {
				_ = closeFn()
			}
			return nil, fmt.Errorf("%w: non-loopback proxy %q forbidden", ErrSealedNetworkDenied, startedURL)
		}
		proxyURL = startedURL
		closeProxy = closeFn
	} else if backend != nil {
		startedURL, closeFn, err := startSealedLoopbackGoProxy(backend, loomDir)
		if err != nil {
			return nil, err
		}
		proxyURL = startedURL
		closeProxy = closeFn
	}
	if closeProxy != nil {
		defer func() { _ = closeProxy() }()
	}

	workDir, err := os.MkdirTemp("", "bashy-fixedpoint-*")
	if err != nil {
		return nil, err
	}
	defer removeWritableTree(workDir)

	stubDir := filepath.Join(workDir, "no-make")
	if err := writeFailingMakeStubs(stubDir); err != nil {
		return nil, err
	}

	ldflags := "-s -w" +
		" -X github.com/bashsharp/bashsharp/cli.bashVersion=5.3.0(1)-bashy-" + version +
		" -X github.com/bashsharp/bashsharp/cli.buildID=" + buildID
	if shellStamp := readShellRuntimeStampFromGoMod(filepath.Join(srcAbs, "go.mod")); shellStamp != "" {
		ldflags += " -X github.com/bashsharp/bashsharp/transpile.ShellRuntimeCommit=" + shellStamp
	}
	if extra := strings.TrimSpace(opts.ExtraLdflags); extra != "" {
		ldflags += " " + extra
	}

	goVer := strings.TrimPrefix(strings.TrimSpace(opts.GoVersion), "go")
	if goVer == "" && buildLock != nil {
		goVer = strings.TrimPrefix(strings.TrimSpace(buildLock.GoVersion), "go")
	}

	dropEnv := map[string]bool{
		"HTTP_PROXY":          true,
		"http_proxy":          true,
		"HTTPS_PROXY":         true,
		"https_proxy":         true,
		"ALL_PROXY":           true,
		"all_proxy":           true,
		goModProxyURLEnv:      true,
		"BASHY_PRODUCTGO_DIR": true,
	}

	effectiveGOPROXY := "off"
	if proxyURL != "" {
		effectiveGOPROXY = proxyURL
	}

	stageNames := [3]string{
		"stage0_prev_to_candidate0",
		"stage1_self_rebuild_1",
		"stage2_self_rebuild_2",
	}
	stageOutputs := [3]string{}
	stageSHAs := [3]string{}
	stageSizes := [3]int64{}
	stages := make([]FixedPointStageReport, 0, 3)
	var peakWorkspaceBytes uint64

	baseInheritedPath := os.Getenv("PATH")
	shadowedPath := stubDir
	if baseInheritedPath != "" {
		shadowedPath = stubDir + string(os.PathListSeparator) + baseInheritedPath
	}

	for s := 0; s < 3; s++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		driverExe := prevExeAbs
		if s > 0 {
			driverExe = stageOutputs[s-1]
		}
		driverHex, _, err := fileSHA256AndSize(driverExe)
		if err != nil {
			return nil, fmt.Errorf("artifact: hash stage %d driver %s: %w", s, driverExe, err)
		}

		stageDir := filepath.Join(workDir, fmt.Sprintf("stage%d", s))
		goCache := filepath.Join(stageDir, "gocache")
		modCache := filepath.Join(stageDir, "gomodcache")
		prodCache := filepath.Join(stageDir, "productgo-cache")
		goTmp := filepath.Join(stageDir, "gotmp")
		binDir := filepath.Join(stageDir, "bin")

		for _, d := range []string{goCache, modCache, prodCache, goTmp, binDir} {
			if err := os.MkdirAll(d, 0o700); err != nil {
				return nil, err
			}
		}
		goCacheStartedEmpty := isEmptyDir(goCache)
		modCacheStartedEmpty := isEmptyDir(modCache)
		if !goCacheStartedEmpty || !modCacheStartedEmpty {
			return nil, fmt.Errorf("artifact: stage %d disposable caches did not start empty", s)
		}

		outBin := filepath.Join(binDir, candidateBinName())
		overrides := map[string]string{
			"PATH":                   shadowedPath,
			"BASHY_OFFLINE":          "1",
			"BASHY_ARTIFACTS_DIR":    artRoot,
			loomRepoEnv:              loomDir,
			"GOTOOLCHAIN":            "local",
			"GOSUMDB":                "off",
			"GONOPROXY":              "",
			"GOPRIVATE":              "",
			"CGO_ENABLED":            "0",
			"GOWORK":                 "off",
			"NO_PROXY":               "*",
			"no_proxy":               "*",
			"HTTP_PROXY":             "",
			"HTTPS_PROXY":            "",
			"GOPROXY":                effectiveGOPROXY,
			"GOCACHE":                goCache,
			"GOMODCACHE":             modCache,
			"BASHY_PRODUCTGO_CACHE":  prodCache,
			"GOTMPDIR":               goTmp,
			"TMPDIR":                 goTmp,
			"TMP":                    goTmp,
			"TEMP":                   goTmp,
			"GOFLAGS":                "-mod=readonly -buildvcs=false",
			"OTEL_TRACES_EXPORTER":   "none",
			"OTEL_METRICS_EXPORTER":  "none",
			"OTEL_LOGS_EXPORTER":     "none",
			"BASHY_EXE":              driverExe,
			"BASHY_SELF_FIXEDPOINT":  "1",
			"BASHY_FIXEDPOINT_STAGE": fmt.Sprintf("%d", s),
		}
		if proxyURL != "" && proxyURL != "off" {
			overrides[goModProxyURLEnv] = proxyURL
		}
		if goVer != "" {
			overrides["BASHY_GO_VERSION"] = goVer
		}
		stageEnv := buildHermeticStageEnv(os.Environ(), overrides, dropEnv)

		var argv []string
		if useProductOverlay {
			argv = []string{
				"go", "run", productgoMain,
				runtime.GOOS, runtime.GOARCH, "0",
				"build", "-trimpath", "-ldflags", ldflags, "-o", outBin, targetPkg,
			}
		} else {
			argv = []string{
				"go", "build", "-trimpath", "-ldflags", ldflags, "-o", outBin, targetPkg,
			}
		}

		stageStart := time.Now()
		cmd := exec.CommandContext(ctx, driverExe, argv...)
		cmd.Dir = srcAbs
		cmd.Env = stageEnv
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		durMS := time.Since(stageStart).Milliseconds()
		if runErr != nil {
			return nil, fmt.Errorf("artifact: fixed-point %s failed: %w\nstdout: %s\nstderr: %s",
				stageNames[s], runErr, strings.TrimSpace(stdout.String()), strings.TrimSpace(stderr.String()))
		}

		outHex, outSize, err := fileSHA256AndSize(outBin)
		if err != nil || outSize == 0 {
			return nil, fmt.Errorf("artifact: fixed-point %s produced no valid output at %s: %v", stageNames[s], outBin, err)
		}
		_ = os.Chmod(outBin, 0o755)

		wsBytes := dirSizeBytes(stageDir)
		if wsBytes > peakWorkspaceBytes {
			peakWorkspaceBytes = wsBytes
		}

		// Reclaim disposable compilation/module caches immediately after measuring stage footprint
		// so subsequent stages physically cannot observe prior-stage caches and disk reserve stays bounded.
		for _, disp := range []string{goCache, modCache, prodCache, goTmp} {
			removeWritableTree(disp)
		}

		stageOutputs[s] = outBin
		stageSHAs[s] = "sha256:" + outHex
		stageSizes[s] = outSize
		stages = append(stages, FixedPointStageReport{
			Stage:          s,
			Name:           stageNames[s],
			DriverExe:      driverExe,
			DriverSHA256:   "sha256:" + driverHex,
			OutputBinary:   outBin,
			OutputSHA256:   stageSHAs[s],
			OutputSize:     outSize,
			DurationMillis: durMS,
			WorkspaceBytes: wsBytes,
			GoCacheEmpty:   goCacheStartedEmpty,
			ModCacheEmpty:  modCacheStartedEmpty,
		})
	}

	identicalBytes, cmpErr := filesByteIdentical(stageOutputs[1], stageOutputs[2])
	if cmpErr != nil {
		return nil, fmt.Errorf("artifact: compare candidate_1 and candidate_2: %w", cmpErr)
	}
	fixedPointMatch := identicalBytes &&
		stageSHAs[1] == stageSHAs[2] &&
		stageSizes[1] == stageSizes[2]

	smokeArgs := slices.Clone(opts.SmokeArgs)
	if len(smokeArgs) == 0 {
		smokeArgs = []string{"--version"}
	}

	report := &FixedPointReportV1{
		SchemaVersion:      FixedPointSchemaV1,
		Platform:           CurrentPlatform(),
		SourceDir:          srcAbs,
		TargetPackage:      targetPkg,
		Version:            version,
		BuildID:            buildID,
		ArtifactRoot:       artRoot,
		LoomDir:            loomDir,
		Sealed:             true,
		ProductOverlayUsed: useProductOverlay,
		ProxyURL:           proxyURL,
		BuildLock:          buildLock,
		Stages:             stages,
		Candidate0SHA256:   stageSHAs[0],
		Candidate0Size:     stageSizes[0],
		Candidate1SHA256:   stageSHAs[1],
		Candidate1Size:     stageSizes[1],
		Candidate2SHA256:   stageSHAs[2],
		Candidate2Size:     stageSizes[2],
		FixedPointMatch:    fixedPointMatch,
		SmokeArgs:          smokeArgs,
		TotalDurationMS:    time.Since(overallStart).Milliseconds(),
		PeakWorkspaceBytes: peakWorkspaceBytes,
	}

	if !fixedPointMatch {
		return report, fmt.Errorf("%w: candidate_1 (%s, %d bytes) != candidate_2 (%s, %d bytes)",
			ErrFixedPointMismatch, stageSHAs[1], stageSizes[1], stageSHAs[2], stageSizes[2])
	}

	smokeTmp := filepath.Join(workDir, "smoke-tmp")
	_ = os.MkdirAll(smokeTmp, 0o700)
	smokeEnv := buildHermeticStageEnv(os.Environ(), map[string]string{
		"PATH":                  shadowedPath,
		"BASHY_OFFLINE":         "1",
		"BASHY_ARTIFACTS_DIR":   artRoot,
		loomRepoEnv:             loomDir,
		"GOTOOLCHAIN":           "local",
		"GOSUMDB":               "off",
		"GONOPROXY":             "",
		"GOPRIVATE":             "",
		"NO_PROXY":              "*",
		"HTTP_PROXY":            "",
		"HTTPS_PROXY":           "",
		"TMPDIR":                smokeTmp,
		"TMP":                   smokeTmp,
		"TEMP":                  smokeTmp,
		"OTEL_TRACES_EXPORTER":  "none",
		"OTEL_METRICS_EXPORTER": "none",
		"OTEL_LOGS_EXPORTER":    "none",
	}, dropEnv)

	smokeCmd := exec.CommandContext(ctx, stageOutputs[2], smokeArgs...)
	smokeCmd.Dir = srcAbs
	smokeCmd.Env = smokeEnv
	smokeOut, smokeErr := smokeCmd.CombinedOutput()
	report.SmokeOutput = strings.TrimSpace(string(smokeOut))
	report.TotalDurationMS = time.Since(overallStart).Milliseconds()
	if smokeErr != nil {
		return report, fmt.Errorf("artifact: candidate_2 smoke %v failed: %w (%s)",
			smokeArgs, smokeErr, report.SmokeOutput)
	}

	if outTarget := strings.TrimSpace(opts.OutputBinary); outTarget != "" {
		outAbs, err := filepath.Abs(outTarget)
		if err != nil {
			return report, err
		}
		if err := os.MkdirAll(filepath.Dir(outAbs), 0o755); err != nil {
			return report, err
		}
		if err := copyVerifiedFileAtomic(stageOutputs[2], outAbs, 0o755); err != nil {
			return report, fmt.Errorf("artifact: write OutputBinary %s: %w", outAbs, err)
		}
		report.OutputBinary = outAbs
	}

	return report, nil
}
