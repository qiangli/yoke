package coord

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// runEvidence creates a fresh directory for each attempt, even when the same
// candidate is retried. The candidate is the caller's source identity, never an
// assertion that a test passed or that a mutable checkout matches that identity.
func runEvidence(child *exec.Cmd, c *Claim, base, candidate string) error {
	if base == "" {
		return child.Run()
	}
	base, err := filepath.Abs(base)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(base, 0700); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(base, "claim-run-")
	if err != nil {
		return err
	}
	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		return err
	}
	defer out.Close()
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		return err
	}
	defer stderr.Close()
	child.Stdout = io.MultiWriter(child.Stdout, out)
	child.Stderr = io.MultiWriter(child.Stderr, stderr)
	child.Env = os.Environ()
	values := map[string]string{"BASHY_CLAIM_EVIDENCE_DIR": dir, "BASHY_CLAIM_CANDIDATE": candidate, "BASHY_CLAIM_EPOCH": strconv.FormatUint(c.Epoch, 10)}
	env := child.Env[:0]
	for _, v := range child.Env {
		k, _, _ := strings.Cut(v, "=")
		if _, ok := values[k]; !ok {
			env = append(env, v)
		}
	}
	child.Env = env
	for k, v := range values {
		child.Env = append(child.Env, k+"="+v)
	}
	record := struct {
		Candidate string    `json:"candidate"`
		Claim     *Claim    `json:"claim"`
		Command   []string  `json:"command"`
		Started   time.Time `json:"started"`
		Finished  time.Time `json:"finished,omitempty"`
		Status    string    `json:"status"`
		Error     string    `json:"error,omitempty"`
	}{Candidate: candidate, Claim: c, Command: child.Args, Started: time.Now().UTC(), Status: "running"}
	save := func() error {
		b, err := json.MarshalIndent(record, "", "  ")
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, "run.json"), b, 0600)
	}
	if err := save(); err != nil {
		return err
	}
	fmt.Fprintln(child.Stderr, "claim: evidence "+dir)
	runErr := child.Run()
	record.Finished = time.Now().UTC()
	record.Status = "completed"
	if runErr != nil {
		record.Status = "failed"
		record.Error = runErr.Error()
	}
	if err := save(); err != nil {
		return fmt.Errorf("claim: evidence finalization failed (child result %v): %w", runErr, err)
	}
	return runErr
}
