package ollama

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	runnerEmbed "github.com/qiangli/yoke/external/ollama/runner_embed"
)

// A CUDA selection whose runner cannot be prepared fails Start before the
// server binds: nothing is listening to answer on CPU.
func TestStartCUDASelectionFailsClosedBeforeBind(t *testing.T) {
	oldSel, oldEnsure := selectRunnerBackend, ensureCUDARunner
	t.Cleanup(func() { selectRunnerBackend, ensureCUDARunner = oldSel, oldEnsure })
	selectRunnerBackend = func(func(string) string) (runnerEmbed.Backend, error) { return runnerEmbed.BackendCUDA, nil }
	ensureCUDARunner = func(context.Context) (runnerEmbed.CUDALaunch, error) {
		return runnerEmbed.CUDALaunch{}, errors.New("runner: CUDA runner unavailable: host CUDA runtime missing libcublas.so")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	t.Setenv("OLLAMA_HOST", addr)
	comp := NewOllamaComponent(&Config{ModelsDir: t.TempDir()}, t.TempDir())
	err = comp.Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "libcublas.so") {
		t.Fatalf("Start err = %v", err)
	}
	if comp.Healthy() || comp.Port() != 0 {
		t.Fatal("component bound or reported healthy after a CUDA refusal")
	}
	if c, err := net.Dial("tcp", addr); err == nil {
		c.Close()
		t.Fatal("something is listening after a CUDA refusal")
	}
}

func TestStartBackendSelectionErrorFailsClosed(t *testing.T) {
	oldSel := selectRunnerBackend
	t.Cleanup(func() { selectRunnerBackend = oldSel })
	selectRunnerBackend = func(func(string) string) (runnerEmbed.Backend, error) {
		return "", errors.New("runner: CUDA runner is published for linux/amd64 only")
	}
	comp := NewOllamaComponent(&Config{}, t.TempDir())
	if err := comp.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "linux/amd64 only") {
		t.Fatalf("Start err = %v", err)
	}
}
