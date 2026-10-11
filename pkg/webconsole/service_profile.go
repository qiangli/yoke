// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"fmt"
	"net"
	"strconv"
	"time"
)

// serviceStartPlan applies the unit's current launch shape for a bare start.
// Any explicit launch flag means the caller is choosing a new shape.
//
// It exists because `bashy app service start` is also run by humans and agents
// after installs. A bare start must not silently replace a paired LAN console
// with a loopback-only one while phone devices still exist.
func serviceStartPlan(current, flags servicePlan, explicit bool) (servicePlan, error) {
	if explicit {
		want := servicePlan{Port: DefaultPort, Bind: "127.0.0.1", Pair: flags.Pair}
		if flags.Port > 0 {
			want.Port = flags.Port
		}
		if flags.Bind != "" {
			want.Bind = flags.Bind
		}
		return want, nil
	}
	if current.Pair {
		return current, nil
	}
	live, pending, err := servicePairingCounts()
	if err != nil {
		return current, fmt.Errorf("apps service start: cannot inspect pairing state, so a bare start would be ambiguous: %w\nre-arm explicitly with: %s",
			err, serviceRearmCommand(current.Port))
	}
	if live > 0 || pending > 0 {
		return current, fmt.Errorf("apps service start: refusing a bare start because pairing state has %d live device(s) and %d pending code(s)\nre-arm explicitly with: %s",
			live, pending, serviceRearmCommand(current.Port))
	}
	return current, nil
}

func servicePairingCounts() (live, pending int, err error) {
	path, err := pairPath()
	if err != nil {
		return 0, 0, err
	}
	st, err := newPairStore(path).load()
	if err != nil {
		return 0, 0, err
	}
	now := time.Now()
	return st.browserDevices(now), st.openTickets(now), nil
}

func serviceRearmCommand(port int) string {
	args := []string{"bashy", "app", "service", "start", "--pair", "--bind", BindLAN}
	if port > 0 && port != DefaultPort {
		args = append(args, "--port", strconv.Itoa(port))
	}
	return shellJoin(args)
}

func shellJoin(args []string) string {
	out := ""
	for i, arg := range args {
		if i > 0 {
			out += " "
		}
		if arg != "" && !containsShellSpecial(arg) {
			out += arg
			continue
		}
		out += strconv.Quote(arg)
	}
	return out
}

func containsShellSpecial(s string) bool {
	for _, r := range s {
		switch r {
		case ' ', '\t', '\r', '\n', '"', '\'', '\\', '$', '`', ';', '&', '|', '<', '>', '*', '?', '(', ')', '[', ']', '{', '}':
			return true
		}
	}
	return false
}

func servicePairingNotice() string {
	live, pending, err := servicePairingCounts()
	if err != nil {
		return "pairing: state unreadable; phone access fails closed: " + err.Error()
	}
	p := servicePlan{Port: DefaultPort, Bind: "127.0.0.1"}
	if unit, uerr := loadServiceUnit(); uerr == nil {
		p = planOf(unit)
	}
	switch {
	case p.Pair:
		// A symbolic bind is shown resolved, with the symbol beside it, so the
		// operator sees both where the phone reaches it now and why that can
		// change without a restart.
		where := net.JoinHostPort(p.Bind, strconv.Itoa(p.Port))
		if p.Bind == BindLAN {
			where = currentPairListenerAddr(where) + " (" + BindLAN + ")"
		}
		return fmt.Sprintf("pairing: armed on %s (%d live device(s), %d pending code(s))",
			where, live, pending)
	case live > 0 || pending > 0:
		return fmt.Sprintf("pairing: disarmed but %d live device(s) and %d pending code(s) exist; re-arm with: %s",
			live, pending, serviceRearmCommand(p.Port))
	default:
		return ""
	}
}

// serviceLauncherPlan resolves the directory before the daemon changes cwd.
// Without an explicit flag the unit's current launcher is kept.
func serviceLauncherPlan(current, launcher string, explicit bool) (string, error) {
	if !explicit {
		launcher = current
	}
	if launcher == "" {
		return "", nil
	}
	return launcherDir(launcher)
}
