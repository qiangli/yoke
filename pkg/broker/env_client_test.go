package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/broker/door"
)

func TestEnvClientGoldens(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(SessionEnv, "")
	t.Setenv(door.PortEnv, "")
	token, err := door.Token()
	if err != nil {
		t.Fatal(err)
	}
	for _, client := range []string{"generic", "openclaw", "codync"} {
		t.Run(client, func(t *testing.T) {
			golden, err := os.ReadFile(filepath.Join("testdata", "env-"+client+".golden"))
			if err != nil {
				t.Fatal(err)
			}
			want := strings.ReplaceAll(string(golden), "{{BASE_URL}}", door.BaseURL())
			want = strings.ReplaceAll(want, "{{TOKEN}}", token)
			if got := runEnv(t, "--client", client); got != want {
				t.Fatalf("llm env --client %s:\n--- got ---\n%s--- want ---\n%s", client, got, want)
			}
		})
	}
}

func TestEnvClientViaMeshUsesDialedLoopbackAddress(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv(SessionEnv, "")
	t.Setenv(door.PortEnv, "31880")
	for _, client := range []string{"generic", "openclaw", "codync"} {
		got := runEnv(t, "--client", client, "--via", "mesh")
		if !strings.Contains(got, "http://127.0.0.1:31880") {
			t.Errorf("%s block does not print the mesh-dialed local address:\n%s", client, got)
		}
	}
}

func TestEnvClientRejectsUnknownClientAndRoute(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	for _, args := range [][]string{{"--client", "mystery"}, {"--via", "wan"}, {"--client", "openclaw", "--json"}} {
		cmd := newEnvCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Errorf("llm env %v unexpectedly succeeded", args)
		}
	}
}
