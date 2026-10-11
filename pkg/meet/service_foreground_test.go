package meet

import (
	"bytes"
	"testing"
)

func TestMeetServiceLifecycleIsForegroundServe(t *testing.T) {
	cmd := NewMeetCmd()
	serve, _, err := cmd.Find([]string{"serve"})
	if err != nil {
		t.Fatal(err)
	}
	if serve == nil {
		t.Fatal("meet serve command missing")
	}
	for _, flag := range []string{"port", "bind"} {
		if serve.Flags().Lookup(flag) == nil {
			t.Errorf("meet serve lost --%s", flag)
		}
	}
	for _, args := range [][]string{{"service", "start"}, {"service", "status"}, {"service", "stop"}} {
		c := NewMeetCmd()
		c.SetArgs(args)
		if err := c.Execute(); err == nil {
			t.Errorf("legacy detached command %v still works", args)
		}
	}
}

func runMeet(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewMeetCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}
