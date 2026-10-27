//go:build windows

// Run explicitly: bashy go run stack_windows.go acceptance_windows.go
// This file adds the isolated real services to the acceptance client.
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func startStack() func() {
	root, err := os.Getwd()
	must(err)
	for _, port := range []int{31890, 8333, 9333, 8080, 8888} {
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		must(err) // Refuse to interfere with an existing service.
		ln.Close()
	}
	home := filepath.Join(root, "home")
	must(os.MkdirAll(filepath.Join(home, "config", "matrix"), 0700))
	config, err := json.Marshal(map[string]any{
		"agent_name": "s515self", "admin_addr": "127.0.0.1:0",
		"mesh_enabled": false, "ssh_enabled": false,
		"mesh_services": []any{map[string]string{"name": "s3", "addr": "127.0.0.1:8333"}},
	})
	must(err)
	must(os.WriteFile(filepath.Join(home, "config", "matrix", "agent.json"), config, 0600))
	var cmds []*exec.Cmd
	var logs []*os.File
	cleanup := func() {
		// The weed binary is a child of the wrapper. Select only its listener;
		// the preflight above established that the port was unused.
		for _, row := range rows() {
			if port(row.LocalPort) == 8333 && row.State == 2 {
				h, err := syscall.OpenProcess(1, false, row.PID)
				if err == nil {
					syscall.TerminateProcess(h, 0)
					syscall.CloseHandle(h)
				}
			}
		}
		for i := len(cmds) - 1; i >= 0; i-- {
			cmds[i].Process.Kill()
			cmds[i].Wait()
		}
		for _, log := range logs {
			log.Close()
		}
	}
	ok := false
	defer func() {
		if !ok {
			cleanup()
		}
	}()
	start := func(name, logname string, env []string, args ...string) {
		log, err := os.Create(logname)
		must(err)
		logs = append(logs, log)
		cmd := exec.Command(filepath.Join(root, name), args...)
		cmd.Env = append(os.Environ(), env...)
		cmd.Stdout, cmd.Stderr = log, log
		must(cmd.Start())
		cmds = append(cmds, cmd)
	}
	start("bashy-s515-fixed.exe", "weed.log", []string{
		"SEAWEEDFS_S3_ACCESS_KEY=s515-proof", "SEAWEEDFS_S3_SECRET_KEY=s515-proof-secret",
	}, "seaweedfs", "serve", "--data", filepath.Join(root, "weed"), "--port", "8333")
	start("outpost-s515.exe", "outpost.log", []string{
		"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(home, "cache"), "APPDATA=" + filepath.Join(home, "appdata"),
	}, "start", "--admin-addr", "127.0.0.1:0")
	for _, addr := range []string{"127.0.0.1:8333", "127.0.0.1:31890"} {
		ready := false
		for i := 0; i < 120; i++ {
			conn, err := net.DialTimeout("tcp", addr, time.Second)
			if err == nil {
				conn.Close()
				ready = true
				break
			}
			time.Sleep(time.Second)
		}
		check(ready, "service %s not ready; inspect weed.log/outpost.log", addr)
	}
	ok = true
	return cleanup
}

func init() { stack = startStack }
