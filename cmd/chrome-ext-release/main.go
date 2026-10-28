// Command chrome-ext-release packs the embedded bashy browser live Chrome
// extension for release: a reproducible zip plus its sha256, and a signed
// CRX3 when a signing key is supplied. Driven by the chrome-ext-* targets
// in dag.md (Sprint 512.3).
//
//	chrome-ext-release [-out DIR] [-key-env NAME]
//
// The signing key is read from the environment variable named by -key-env
// (the dag target fills it from `bashy secret`); it is never a file in the
// repository.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/qiangli/yoke/pkg/browser/live"
)

func main() {
	out := flag.String("out", "dist/chrome-ext", "release directory")
	keyEnv := flag.String("key-env", "", "environment variable holding the CRX signing key PEM (optional)")
	flag.Parse()
	if err := run(*out, *keyEnv); err != nil {
		fmt.Fprintln(os.Stderr, "chrome-ext-release:", err)
		os.Exit(1)
	}
}

func run(out, keyEnv string) error {
	ver, err := live.CheckReleaseVersion()
	if err != nil {
		return err
	}
	z, err := live.PackExtensionZip()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	base := filepath.Join(out, "bashy-live-"+ver)
	if err := writeWithSum(base+".zip", z); err != nil {
		return err
	}
	fmt.Printf("version %s\nzip     %s.zip\n", ver, base)
	if keyEnv == "" {
		return nil
	}
	pemKey := os.Getenv(keyEnv)
	if pemKey == "" {
		return fmt.Errorf("-key-env %s is empty", keyEnv)
	}
	crx, id, err := live.PackCRX3(z, []byte(pemKey))
	if err != nil {
		return err
	}
	if err := writeWithSum(base+".crx", crx); err != nil {
		return err
	}
	fmt.Printf("crx     %s.crx\nid      %s\n", base, id)
	return nil
}

func writeWithSum(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	line := hex.EncodeToString(sum[:]) + "  " + filepath.Base(path) + "\n"
	return os.WriteFile(path+".sha256", []byte(line), 0o644)
}
