package foremancmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/foreman"
)

func runREPLWithFlags(rc *tool.RunContext, flags map[string]string, args []string) int {
	goal := strings.TrimSpace(flags["goal"])
	if goal == "" && len(args) > 0 {
		goal = strings.Join(args, " ")
	}
	if goal == "" {
		goal = "interactive foreman session"
	}
	s, err := foreman.Start(rc.Ctx, foreman.Options{
		ID:     flags["id"],
		Goal:   goal,
		Agent:  flags["agent"],
		Role:   flags["role"],
		Cwd:    rc.Dir,
		Runner: runner,
	})
	if err != nil {
		return fail(rc, flags["json"] == "true", err)
	}
	defer s.Close()
	fmt.Fprintf(rc.Out, "foreman %s\n", s.State().ID)
	if err := driveLines(rc.Ctx, rc.In, rc.Out, s); err != nil {
		return fail(rc, flags["json"] == "true", err)
	}
	return 0
}

func driveLines(ctx context.Context, in io.Reader, out io.Writer, s *foreman.Session) error {
	sc := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "foreman> ")
		if !sc.Scan() {
			break
		}
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if err := dispatchLine(ctx, out, s, line); err != nil {
			return err
		}
		if s.State().Stopped {
			break
		}
	}
	return sc.Err()
}

func dispatchLine(ctx context.Context, out io.Writer, s *foreman.Session, line string) error {
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return nil
	}
	switch fields[0] {
	case "status":
		st := s.State()
		fmt.Fprintf(out, "%s\t%s\t%s\n", st.ID, st.Status, st.Goal)
	case "pause", "resume", "stop":
		if err := s.Apply(ctx, foreman.Command{Verb: fields[0]}); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s\n", s.State().Status)
	case "tell":
		msg := strings.TrimSpace(strings.TrimPrefix(line, "tell"))
		if msg == "" {
			return fmt.Errorf("foreman: tell message required")
		}
		if err := s.Apply(ctx, foreman.Command{Verb: foreman.CommandTell, Message: msg}); err != nil {
			return err
		}
	case "skip":
		c := foreman.Command{Verb: foreman.CommandSkip}
		if len(fields) > 1 {
			c.Target = fields[1]
		}
		if err := s.Apply(ctx, c); err != nil {
			return err
		}
	case "prio":
		if len(fields) < 2 || len(fields) > 3 {
			return fmt.Errorf("foreman: prio requires [target] priority")
		}
		c := foreman.Command{Verb: foreman.CommandPrio, Priority: fields[len(fields)-1]}
		if len(fields) == 3 {
			c.Target = fields[1]
		}
		if err := s.Apply(ctx, c); err != nil {
			return err
		}
	default:
		if err := s.Apply(ctx, foreman.Command{Verb: foreman.CommandTell, Message: line}); err != nil {
			return err
		}
	}
	return s.Store().SaveState(s.State())
}
