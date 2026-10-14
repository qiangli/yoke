// Package grype runs anchore/grype (vulnerability scanner for container images and
// filesystems) as a managed external binary (pkg/binmgr): downloaded → sha256-verified →
// cached, never compiled in. anchore/grype is Apache-2.0.
package grype

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// DefaultVersion is the pinned release version for grype.
const DefaultVersion = "v0.120.1"

// StatusSkippedOffline is the explicit status reported when offline without a cached DB.
const StatusSkippedOffline = "skipped: offline, no cached vulnerability database"

// ErrOfflineNoDB is the sentinel error returned when grype is skipped due to missing cached DB.
var ErrOfflineNoDB = errors.New(StatusSkippedOffline)

// Spec is the binmgr GitHub spec for grype.
// anchore/grype ships per-platform archives:
// grype_<ver>_darwin_amd64.tar.gz, grype_<ver>_darwin_arm64.tar.gz,
// grype_<ver>_linux_amd64.tar.gz, grype_<ver>_linux_arm64.tar.gz,
// grype_<ver>_windows_amd64.zip.
// The executable lives at the root of the archive as "grype" (or "grype.exe" on Windows).
func Spec(version string) binmgr.GitHubSpec {
	if version == "" {
		version = DefaultVersion
	}
	return binmgr.GitHubSpec{
		Name:       "grype",
		Repo:       "anchore/grype",
		Version:    version,
		Member:     "grype",
		AssetMatch: assetMatch,
	}
}

// assetMatch maps Go's goos/goarch to grype's release archive assets.
func assetMatch(name, goos, goarch string) bool {
	n := strings.ToLower(name)
	if !strings.HasPrefix(n, "grype_") {
		return false
	}
	if strings.Contains(n, "checksum") || strings.HasSuffix(n, ".sigstore.json") {
		return false
	}
	if !strings.Contains(n, "_"+goos+"_") {
		return false
	}
	if !strings.Contains(n, "_"+goarch) {
		return false
	}
	if goos == "windows" {
		return strings.HasSuffix(n, ".zip")
	}
	return strings.HasSuffix(n, ".tar.gz")
}

// Ensure fetches (if needed) the grype binary and returns its cached path.
func Ensure(ctx context.Context, version string) (string, error) {
	tool, err := binmgr.ResolveGitHub(ctx, Spec(version))
	if err != nil {
		return "", fmt.Errorf("grype: resolve: %w", err)
	}
	return binmgr.Ensure(ctx, tool)
}

// DBCacheDir returns the directory used for the grype vulnerability database cache,
// respecting $GRYPE_DB_CACHE_DIR, $BASHY_HOME, or the existing binmgr cache root.
func DBCacheDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("GRYPE_DB_CACHE_DIR")); dir != "" {
		return dir, nil
	}
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "cache", "grype", "db"), nil
	}
	root, err := binmgr.CacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "grype", "db"), nil
}

// HasCachedDB reports whether a downloaded vulnerability database exists in dbDir.
func HasCachedDB(dbDir string) bool {
	if dbDir == "" {
		return false
	}
	if fi, err := os.Stat(filepath.Join(dbDir, "vulnerability.db")); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return true
	}
	matches, err := filepath.Glob(filepath.Join(dbDir, "*", "vulnerability.db"))
	if err == nil {
		for _, m := range matches {
			if fi, err := os.Stat(m); err == nil && !fi.IsDir() && fi.Size() > 0 {
				return true
			}
		}
	}
	return false
}

// DBEnv returns the environment variables configured with the DB cache directory
// and auto-update disabled when a cached DB exists.
func DBEnv(dbDir string) []string {
	env := notrack.Env(os.Environ())
	env = append(env, "GRYPE_DB_CACHE_DIR="+dbDir)
	if HasCachedDB(dbDir) {
		env = append(env, "GRYPE_DB_AUTO_UPDATE=false")
	}
	return env
}

// ChildEnv returns env with grype DB settings and tracking disabled.
func ChildEnv(env []string) []string {
	dir, err := DBCacheDir()
	if err == nil {
		env = append(env, "GRYPE_DB_CACHE_DIR="+dir)
		if HasCachedDB(dir) {
			env = append(env, "GRYPE_DB_AUTO_UPDATE=false")
		}
	}
	return notrack.Env(env)
}

// EnsureDB ensures the vulnerability database is present in the cache.
// If a cached DB already exists, auto-update is disabled and the cached DB is reused.
// When offline (BASHY_OFFLINE=1) and no cached DB exists, or when the DB download fails,
// grype is skipped with StatusSkippedOffline and ErrOfflineNoDB.
func EnsureDB(ctx context.Context, grypeBin string) (string, error) {
	dbDir, err := DBCacheDir()
	if err != nil {
		return "", fmt.Errorf("grype: db cache dir: %w", err)
	}

	if HasCachedDB(dbDir) {
		return dbDir, nil
	}

	if binmgr.Offline() {
		return StatusSkippedOffline, ErrOfflineNoDB
	}

	if err := os.MkdirAll(dbDir, 0755); err != nil {
		return StatusSkippedOffline, fmt.Errorf("%w: mkdir: %v", ErrOfflineNoDB, err)
	}

	if grypeBin == "" {
		var gerr error
		grypeBin, gerr = Ensure(ctx, "")
		if gerr != nil {
			return StatusSkippedOffline, fmt.Errorf("%w: %v", ErrOfflineNoDB, gerr)
		}
	}

	cmd := binmgr.Command(ctx, grypeBin, "db", "update")
	cmd.Env = append(notrack.Env(os.Environ()),
		"GRYPE_DB_CACHE_DIR="+dbDir,
		"GRYPE_DB_AUTO_UPDATE=true",
	)
	if out, err := cmd.CombinedOutput(); err != nil || !HasCachedDB(dbDir) {
		return StatusSkippedOffline, fmt.Errorf("%w: %v (%s)", ErrOfflineNoDB, err, strings.TrimSpace(string(out)))
	}

	return dbDir, nil
}

// Run executes grype with args, ensuring the binary and database cache first.
// If skipped due to offline / missing DB, it returns (StatusSkippedOffline, ErrOfflineNoDB).
func Run(ctx context.Context, args ...string) (string, error) {
	bin, err := Ensure(ctx, "")
	if err != nil {
		if binmgr.Offline() {
			return StatusSkippedOffline, ErrOfflineNoDB
		}
		return "", err
	}
	dbDir, err := EnsureDB(ctx, bin)
	if err != nil {
		if errors.Is(err, ErrOfflineNoDB) {
			return StatusSkippedOffline, err
		}
		return "", err
	}
	c := binmgr.Command(ctx, bin, args...)
	c.Env = DBEnv(dbDir)
	out, err := c.CombinedOutput()
	return string(out), err
}

// NewGrypeCmd builds the `bashy grype` front-door.
func NewGrypeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "grype",
		Short: "Run anchore/grype (managed external binary)",
		Long: `grype scans container images and filesystems for vulnerabilities — downloaded,
sha256-verified, and cached by binmgr (not compiled in). $GRYPE_VERSION pins the release.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		RunE: func(cmd *cobra.Command, args []string) error {
			bin, err := Ensure(cmd.Context(), os.Getenv("GRYPE_VERSION"))
			if err != nil {
				return err
			}
			dbDir, err := EnsureDB(cmd.Context(), bin)
			if err != nil {
				if errors.Is(err, ErrOfflineNoDB) {
					fmt.Fprintln(cmd.ErrOrStderr(), StatusSkippedOffline)
					return ErrOfflineNoDB
				}
				return err
			}
			c := binmgr.Command(cmd.Context(), bin, args...)
			c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
			c.Env = DBEnv(dbDir)
			return c.Run()
		},
	}
}
