package execlog

import (
	"os"
	"path/filepath"
	"strings"
)

// DefaultRoot is where exec history episodes live — the ONE place that path
// is resolved, shared by the ExecHandler middleware that writes it and every
// `graph` verb that reads it:
//
//	$BASHY_EXECHIST=<path>  an explicit store (a boolean value is the on/off
//	                        switch, not a path, and falls through)
//	$BASHY_HOME/exec        the whole bashy home relocated
//	~/.bashy/exec           the default
//	$TMPDIR/bashy-exec      when no home can be determined
func DefaultRoot() string {
	v := strings.TrimSpace(os.Getenv("BASHY_EXECHIST"))
	switch strings.ToLower(v) {
	case "", "1", "true", "on", "yes", "0", "false", "off", "no":
	default:
		return v
	}
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "exec")
	}
	if h, err := os.UserHomeDir(); err == nil && h != "" {
		return filepath.Join(h, ".bashy", "exec")
	}
	return filepath.Join(os.TempDir(), "bashy-exec")
}
