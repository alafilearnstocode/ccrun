package cgroup

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const cgroupRoot = "/sys/fs/cgroup"

// Cgroup is a cgroup v2 created by the parent (host-side) process. The
// container is cloned directly into it via CLONE_INTO_CGROUP, so the limits
// apply from the container's first instruction.
type Cgroup struct {
	path string
	dir  *os.File
}

func isCgroupV2() bool {
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupRoot, &st); err != nil {
		return false
	}
	return st.Type == unix.CGROUP2_SUPER_MAGIC
}

// New creates /sys/fs/cgroup/ccrun/<pid> with the given limits. It must be
// called from the host side, before the container's root is changed.
func New(memBytes int64, cpuPct int) (*Cgroup, error) {
	if !isCgroupV2() {
		return nil, fmt.Errorf("cgroup v2 is not mounted at %s", cgroupRoot)
	}

	parent := filepath.Join(cgroupRoot, "ccrun")
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", parent, err)
	}
	// Controllers must be enabled in every ancestor's subtree_control before
	// memory.max / cpu.max exist in a child.
	for _, p := range []string{cgroupRoot, parent} {
		if err := enableControllers(p, "memory", "cpu"); err != nil {
			return nil, err
		}
	}

	path := filepath.Join(parent, strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir cgroup: %w", err)
	}
	cg := &Cgroup{path: path}

	if err := cg.setLimits(memBytes, cpuPct); err != nil {
		cg.Remove()
		return nil, err
	}

	dir, err := os.Open(path)
	if err != nil {
		cg.Remove()
		return nil, err
	}
	cg.dir = dir
	return cg, nil
}

func (c *Cgroup) setLimits(memBytes int64, cpuPct int) error {
	mem := "max"
	if memBytes > 0 {
		mem = strconv.FormatInt(memBytes, 10)
	}
	if err := c.write("memory.max", mem); err != nil {
		return err
	}
	if memBytes > 0 {
		// Without this the kernel swaps instead of enforcing the limit.
		// memory.swap.max is absent when swap accounting is off; that's fine.
		if err := c.write("memory.swap.max", "0"); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		// Kill the whole container on OOM, like Docker does, rather than
		// leaving it running with one process missing.
		if err := c.write("memory.oom.group", "1"); err != nil {
			return err
		}
	}

	const period = 100000 // 100ms
	cpu := "max"
	if cpuPct > 0 && cpuPct < 100 {
		quota := period * cpuPct / 100
		if quota < 1000 {
			quota = 1000
		}
		cpu = fmt.Sprintf("%d %d", quota, period)
	}
	return c.write("cpu.max", cpu)
}

func (c *Cgroup) write(file, val string) error {
	if err := os.WriteFile(filepath.Join(c.path, file), []byte(val), 0o644); err != nil {
		return fmt.Errorf("set %s: %w", file, err)
	}
	return nil
}

// FD returns a directory fd for SysProcAttr.CgroupFD.
func (c *Cgroup) FD() int { return int(c.dir.Fd()) }

// OOMKilled reports whether the kernel OOM-killed anything in the cgroup.
func (c *Cgroup) OOMKilled() bool {
	f, err := os.Open(filepath.Join(c.path, "memory.events"))
	if err != nil {
		return false
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		fields := strings.Fields(s.Text())
		if len(fields) == 2 && fields[0] == "oom_kill" {
			n, _ := strconv.Atoi(fields[1])
			return n > 0
		}
	}
	return false
}

// Remove deletes the cgroup. It must be called from outside the cgroup after
// the container has exited; the kernel can take a moment to release the
// exited processes, so it retries briefly on EBUSY.
func (c *Cgroup) Remove() error {
	if c.dir != nil {
		c.dir.Close()
	}
	var err error
	for i := 0; i < 50; i++ {
		if err = unix.Rmdir(c.path); err == nil || errors.Is(err, unix.ENOENT) {
			return nil
		}
		if !errors.Is(err, unix.EBUSY) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("remove cgroup %s: %w", c.path, err)
}

func enableControllers(path string, controllers ...string) error {
	cur, err := os.ReadFile(filepath.Join(path, "cgroup.subtree_control"))
	if err != nil {
		return err
	}
	enabled := strings.Fields(string(cur))
	var want []string
	for _, c := range controllers {
		found := false
		for _, e := range enabled {
			if e == c {
				found = true
				break
			}
		}
		if !found {
			want = append(want, "+"+c)
		}
	}
	if len(want) == 0 {
		return nil
	}
	if err := os.WriteFile(filepath.Join(path, "cgroup.subtree_control"), []byte(strings.Join(want, " ")), 0o644); err != nil {
		return fmt.Errorf("enable %v in %s: %w", want, path, err)
	}
	return nil
}
