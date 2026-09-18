package engine

import "testing"

func TestShouldEnsureMachine(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{"default", nil, true},
		{"run", []string{"run", "alpine"}, true},
		{"info", []string{"info"}, true},
		{"help flag", []string{"--help"}, false},
		{"version flag", []string{"--version"}, false},
		{"machine", []string{"machine", "list"}, false},
		{"help command", []string{"help", "run"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldEnsureMachine(tt.args); got != tt.want {
				t.Fatalf("shouldEnsureMachine(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}
