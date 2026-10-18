package dblifecycle

import "github.com/qiangli/yoke/pkg/dbstore"

// Operation is the lifecycle declaration consumed by CLI, MCP's registered
// command, owner IPC and capability output. Category stories add their own
// declarations; they do not infer mutation from an SQL string.
type Operation struct {
	Name          string   `json:"op"`
	MutationClass string   `json:"mutation_class"`
	Effects       []string `json:"effects"`
	Hold          string   `json:"hold"`
	Atomic        string   `json:"atomic"`
	DryRun        string   `json:"dry_run"`
	Modes         []string `json:"modes"`
}

func Spec(op string, mode dbstore.Mode) Operation {
	s := Operation{Name: op, MutationClass: "read", Hold: "none", Atomic: "op", DryRun: "local-preview", Modes: []string{"memory", "persistent"}}
	if mode == dbstore.Memory {
		s.Effects = []string{"db.read"}
	} else {
		s.Effects = []string{"fs.read", "db.read"}
	}
	switch op {
	case "create":
		s.MutationClass = "write"
		if mode == dbstore.Memory {
			s.Effects = []string{"db.write"}
		} else {
			s.Effects = []string{"fs.write", "db.write"}
		}
	case "drop":
		s.MutationClass = "destructive"
		s.Hold = "exclusive"
		if mode == dbstore.Memory {
			s.Effects = []string{"db.delete", "fs.write"}
		} else {
			s.Effects = []string{"fs.delete", "db.delete", "fs.write"}
		}
	case "restore":
		s.MutationClass = "destructive"
		s.Hold = "exclusive"
		s.Effects = []string{"fs.read", "fs.write", "db.write"}
	case "backup":
		s.Effects = []string{"db.read", "fs.write"}
	case "register", "unregister":
		s.MutationClass = "write"
		s.Effects = []string{"fs.write"}
	case "hold", "release":
		s.MutationClass = "write"
		s.Hold = "exclusive"
		s.Effects = []string{"fs.write"}
	case "session.start":
		s.MutationClass = "write"
		s.Effects = []string{"proc.spawn", "fs.write"}
	case "session.stop":
		s.MutationClass = "destructive"
		s.Effects = []string{"proc.stop", "db.delete", "fs.write"}
	case "session.attach", "session.show", "session.list":
		s.Effects = []string{"fs.read"}
	}
	return s
}
