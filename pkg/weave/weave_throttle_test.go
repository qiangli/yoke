package weave

import "testing"

func TestWeaveClassifyThrottle(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		exitCode   int
		logTail    string
		want       bool
		wantSignal bool
	}{
		{
			name:       "claude usage limit",
			tool:       "claude",
			exitCode:   1,
			logTail:    "Error: usage limit reached. Please try again later.",
			want:       true,
			wantSignal: true,
		},
		{
			name:       "codex rate limit and 429",
			tool:       "codex",
			exitCode:   1,
			logTail:    "Request failed: rate limit exceeded (HTTP 429).",
			want:       true,
			wantSignal: true,
		},
		{
			name:     "normal completion",
			tool:     "codex",
			exitCode: 0,
			logTail:  "All done.",
			want:     false,
		},
		{
			name:     "ordinary crash",
			tool:     "claude",
			exitCode: 1,
			logTail:  "panic: nil pointer",
			want:     false,
		},
		{
			name:       "case insensitive",
			tool:       "opencode",
			exitCode:   1,
			logTail:    "USAGE LIMIT REACHED",
			want:       true,
			wantSignal: true,
		},
		{
			name:     "empty log",
			tool:     "codex",
			exitCode: 1,
			logTail:  "",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, signal := weaveClassifyThrottle(tt.tool, tt.exitCode, tt.logTail)
			if got != tt.want {
				t.Fatalf("weaveClassifyThrottle() throttled = %v, want %v (signal %q)", got, tt.want, signal)
			}
			if tt.wantSignal && signal == "" {
				t.Fatalf("weaveClassifyThrottle() signal is empty")
			}
			if !tt.want && signal != "" {
				t.Fatalf("weaveClassifyThrottle() signal = %q, want empty", signal)
			}
		})
	}
}
