package tools

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

// SystemInfo reports basic hardware/OS facts about the machine.
func (t *Tools) SystemInfo() (string, error) {
	hostname, _ := os.Hostname()
	var b strings.Builder
	fmt.Fprintf(&b, "hostname: %s\n", hostname)
	fmt.Fprintf(&b, "os: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	fmt.Fprintf(&b, "go: %s\n", runtime.Version())
	fmt.Fprintf(&b, "cpus: %d\n", runtime.NumCPU())
	if mem, err := memoryInfo(); err == nil {
		fmt.Fprintf(&b, "memory: %s\n", mem)
	}
	return b.String(), nil
}

func memoryInfo() (string, error) {
	switch runtime.GOOS {
	case "linux":
		return memoryInfoLinux()
	case "darwin":
		return memoryInfoDarwin()
	default:
		return "unknown", nil
	}
}

func memoryInfoLinux() (string, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return "", err
	}
	var totalKB, availKB int64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			fmt.Sscanf(f[1], "%d", &totalKB)
		case "MemAvailable:":
			fmt.Sscanf(f[1], "%d", &availKB)
		}
	}
	return fmt.Sprintf("%d MB total, %d MB available", totalKB/1024, availKB/1024), nil
}

func memoryInfoDarwin() (string, error) {
	res, err := execOutput("sysctl", "-n", "hw.memsize")
	if err != nil {
		return "", err
	}
	var bytes int64
	fmt.Sscanf(strings.TrimSpace(res), "%d", &bytes)
	return fmt.Sprintf("%d MB total", bytes/1024/1024), nil
}
