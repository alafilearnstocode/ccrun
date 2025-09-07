package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

// cgroup2 magic = 0x63677270
func isCgroupV2() bool {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &st); err != nil {
		return false
	}
	return st.Type == 0x63677270
}

func EnsureMount() error {
	if isCgroupV2() {
		return nil
	}
	if err := unix.Mount("none", cgroupRoot, "cgroup2", 0, ""); err != nil {
		return fmt.Errorf("mount cgroup2: %w", err)
	}
	return nil
}

func SetupAndEnter(memBytes int64, cpuPct int) (string, error) {
	if err := EnsureMount(); err != nil {
		return "", err
	}

	name := fmt.Sprintf("ccrun-%d", os.Getpid())
	path := filepath.Join(cgroupRoot, name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("mkdir cgroup: %w", err)
	}

	// memory.max
	if memBytes <= 0 {
		if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte("max"), 0o644); err != nil {
			return "", fmt.Errorf("set memory.max: %w", err)
		}
	} else {
		if err := os.WriteFile(filepath.Join(path, "memory.max"), []byte(strconv.FormatInt(memBytes, 10)), 0o644); err != nil {
			return "", fmt.Errorf("set memory.max: %w", err)
		}
	}

	// cpu.max
	const period = 100000 // 100ms
	var cpuVal string
	if cpuPct <= 0 || cpuPct >= 100 {
		cpuVal = "max"
	} else {
		quota := period * cpuPct / 100
		if quota < 1000 {
			quota = 1000
		}
		cpuVal = fmt.Sprintf("%d %d", quota, period)
	}
	if err := os.WriteFile(filepath.Join(path, "cpu.max"), []byte(cpuVal), 0o644); err != nil {
		return "", fmt.Errorf("set cpu.max: %w", err)
	}

	// join cgroup
	self := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(filepath.Join(path, "cgroup.procs"), []byte(self), 0o644); err != nil {
		return "", fmt.Errorf("join cgroup: %w", err)
	}

	return path, nil
}

func Cleanup(path string) {
	_ = os.Remove(path)
}
