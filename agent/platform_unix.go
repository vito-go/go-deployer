//go:build !windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func getSystemMemory() uint64 {
	// Try macOS sysctl first
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			return v
		}
	}
	// Try Linux /proc/meminfo
	out, err = exec.Command("sh", "-c", "grep MemTotal /proc/meminfo | awk '{print $2}'").Output()
	if err == nil {
		if v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64); err == nil {
			return v * 1024 // KB → bytes
		}
	}
	return 0
}

func getSystemMemUsed() uint64 {
	// macOS: vm_stat
	out, err := exec.Command("sh", "-c", "vm_stat | head -5").Output()
	if err == nil {
		var active, wired, speculative uint64
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(line, ":")
			if len(fields) != 2 {
				continue
			}
			val := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(fields[1]), "."))
			v, _ := strconv.ParseUint(val, 10, 64)
			key := strings.TrimSpace(fields[0])
			switch {
			case strings.Contains(key, "active"):
				active = v
			case strings.Contains(key, "wired"):
				wired = v
			case strings.Contains(key, "speculative"):
				speculative = v
			}
		}
		return (active + wired + speculative) * 4096
	}
	// Linux: /proc/meminfo
	out, err = exec.Command("sh", "-c", "grep -E '^(MemTotal|MemAvailable):' /proc/meminfo | awk '{print $2}'").Output()
	if err == nil {
		lines := strings.Split(strings.TrimSpace(string(out)), "\n")
		if len(lines) >= 2 {
			total, _ := strconv.ParseUint(lines[0], 10, 64)
			avail, _ := strconv.ParseUint(lines[1], 10, 64)
			return (total - avail) * 1024
		}
	}
	return 0
}

func collectProcessStats(pid int) (cpuPercent float64, memRSS uint64) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "%cpu,rss").Output()
	if err != nil {
		return 0, 0
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return 0, 0
	}
	fields := strings.Fields(lines[1])
	if len(fields) >= 2 {
		cpuPercent, _ = strconv.ParseFloat(fields[0], 64)
		rssKB, _ := strconv.ParseUint(fields[1], 10, 64)
		memRSS = rssKB * 1024 // KB → bytes
	}
	return
}

func startBackgroundProcess(binPath, appArgs, workDir string) error {
	args := splitArgs(appArgs)
	cmd := exec.Command(binPath, args...)
	cmd.Dir = workDir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	// Detach stdio to /dev/null so the child survives after parent exits
	devNull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if devNull != nil {
		cmd.Stdin = devNull
		cmd.Stdout = devNull
		cmd.Stderr = devNull
		defer devNull.Close()
	}
	return cmd.Start()
}

func killProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}

func selfTerminate() {
	syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
}

func shellCommand(cmdStr string) *exec.Cmd {
	return exec.Command("sh", "-c", cmdStr)
}

func shellComplete(word, dir string, isCommand bool) []string {
	var out []byte
	if isCommand {
		out, _ = exec.Command("bash", "-c", fmt.Sprintf("compgen -c -- %q 2>/dev/null | head -20", word)).Output()
	} else {
		out, _ = exec.Command("bash", "-c", fmt.Sprintf("cd %q && compgen -f -- %q 2>/dev/null | head -20", dir, word)).Output()
	}
	var candidates []string
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			candidates = append(candidates, l)
		}
	}
	return candidates
}
