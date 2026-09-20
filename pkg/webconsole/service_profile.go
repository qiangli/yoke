// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/qiangli/yoke/pkg/svcd"
)

const serviceProfileSchema = "bashy-apps-service-profile-v1"

// serviceProfile remembers the launch shape outpost armed for the Apps daemon.
//
// It exists because `bashy app service start` is also run by humans and agents
// after installs. A bare start must not silently replace a paired LAN console
// with a loopback-only one while phone devices still exist.
type serviceProfile struct {
	Schema  string    `json:"schema"`
	Pair    bool      `json:"pair"`
	Bind    string    `json:"bind"`
	Port    int       `json:"port"`
	Updated time.Time `json:"updated"`
}

func serviceProfilePath() (string, error) {
	dir, err := serviceDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "service.profile.json"), nil
}

func loadServiceProfile() (serviceProfile, bool, error) {
	path, err := serviceProfilePath()
	if err != nil {
		return serviceProfile{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return serviceProfile{}, false, nil
		}
		return serviceProfile{}, false, err
	}
	var p serviceProfile
	if err := json.Unmarshal(raw, &p); err != nil {
		return serviceProfile{}, false, fmt.Errorf("apps service profile %s is unreadable: %w", path, err)
	}
	if p.Schema != serviceProfileSchema {
		return serviceProfile{}, false, fmt.Errorf("apps service profile %s has schema %q, want %q", path, p.Schema, serviceProfileSchema)
	}
	if p.Port <= 0 {
		p.Port = DefaultPort
	}
	if p.Bind == "" {
		p.Bind = "127.0.0.1"
	}
	return p, true, nil
}

func saveServiceProfile(opt svcd.Options, pair bool) error {
	path, err := serviceProfilePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	p := serviceProfile{
		Schema:  serviceProfileSchema,
		Pair:    pair,
		Bind:    effectiveServiceBind(opt),
		Port:    effectiveServicePort(opt),
		Updated: time.Now().UTC(),
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func effectiveServicePort(opt svcd.Options) int {
	if opt.Port > 0 {
		return opt.Port
	}
	return DefaultPort
}

func effectiveServiceBind(opt svcd.Options) string {
	if opt.Bind != "" {
		return opt.Bind
	}
	return "127.0.0.1"
}

func profileOptions(p serviceProfile) svcd.Options {
	return svcd.Options{Bind: p.Bind, Port: p.Port}
}

// serviceStartPlan applies the persisted launch profile for a bare start. Any
// explicit launch flag means the caller is choosing a new profile.
func serviceStartPlan(opt svcd.Options, pair, explicit bool) (svcd.Options, bool, error) {
	if explicit {
		return opt, pair, nil
	}
	if p, ok, err := loadServiceProfile(); err != nil {
		return opt, pair, err
	} else if ok {
		return profileOptions(p), p.Pair, nil
	}
	live, pending, err := servicePairingCounts()
	if err != nil {
		return opt, pair, fmt.Errorf("apps service start: cannot inspect pairing state, so a bare start would be ambiguous: %w\nre-arm explicitly with: %s",
			err, serviceRearmCommand(effectiveServicePort(opt)))
	}
	if live > 0 || pending > 0 {
		return opt, pair, fmt.Errorf("apps service start: refusing a bare start because pairing state has %d live device(s) and %d pending code(s)\nre-arm explicitly with: %s",
			live, pending, serviceRearmCommand(effectiveServicePort(opt)))
	}
	return opt, pair, nil
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
	return len(st.liveDevices(now)), st.openTickets(now), nil
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
	p, ok, perr := loadServiceProfile()
	if perr != nil {
		return "pairing: profile unreadable; re-arm with: " + serviceRearmCommand(DefaultPort)
	}
	switch {
	case ok && p.Pair:
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
			live, pending, serviceRearmCommand(DefaultPort))
	default:
		return ""
	}
}
