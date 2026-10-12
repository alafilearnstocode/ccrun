//go:build linux

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests build ccrun, pull alpine from Docker Hub, and run real
// containers. They need root, cgroup v2 and network access:
//
//	sudo -E env "PATH=$PATH" CCRUN_INTEGRATION=1 go test ./cmd/ccrun -v
func setup(t *testing.T) (bin string, env []string) {
	t.Helper()
	if os.Getenv("CCRUN_INTEGRATION") != "1" {
		t.Skip("set CCRUN_INTEGRATION=1 to run")
	}
	if os.Geteuid() != 0 {
		t.Skip("needs root for cgroups")
	}

	dir := t.TempDir()
	bin = filepath.Join(dir, "ccrun")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	env = append(os.Environ(), "CCRUN_IMAGES_DIR="+filepath.Join(dir, "images"))
	return bin, env
}

func ccrun(t *testing.T, bin string, env []string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee):
		code = ee.ExitCode()
	default:
		t.Fatal(err)
	}
	return code, o.String(), e.String()
}

func TestContainer(t *testing.T) {
	bin, env := setup(t)

	// tail buffers its whole input when there are no newlines, so this
	// allocates ~128MB inside the container.
	const hog = "set -o pipefail; head -c 128m /dev/zero | tail > /dev/null && echo survived"

	t.Run("runs in image rootfs with host fs detached", func(t *testing.T) {
		code, out, errOut := ccrun(t, bin, env, "run", "--hostname", "box", "alpine", "--",
			"sh", "-c", "cat /etc/alpine-release && hostname && ls -a / && echo pid=$$")
		if code != 0 {
			t.Fatalf("exit %d\n%s", code, errOut)
		}
		if !strings.Contains(out, "box\n") {
			t.Errorf("hostname not set:\n%s", out)
		}
		if strings.Contains(out, ".pivot_old") {
			t.Errorf("old root still visible:\n%s", out)
		}
	})

	t.Run("memory limit OOM-kills a hog", func(t *testing.T) {
		code, out, errOut := ccrun(t, bin, env, "run", "--mem", "32", "alpine", "--", "sh", "-c", hog)
		if code != 137 {
			t.Fatalf("exit = %d, want 137 (SIGKILL)\nstdout: %s\nstderr: %s", code, out, errOut)
		}
		if strings.Contains(out, "survived") {
			t.Fatal("process survived despite 32MB limit")
		}
		if !strings.Contains(errOut, "OOM-killed") {
			t.Errorf("stderr missing OOM report: %s", errOut)
		}
	})

	t.Run("same workload passes with enough memory", func(t *testing.T) {
		code, out, errOut := ccrun(t, bin, env, "run", "--mem", "512", "alpine", "--", "sh", "-c", hog)
		if code != 0 || !strings.Contains(out, "survived") {
			t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, out, errOut)
		}
	})

	t.Run("cgroup is cleaned up", func(t *testing.T) {
		left, err := filepath.Glob("/sys/fs/cgroup/ccrun/*")
		if err != nil {
			t.Fatal(err)
		}
		var dirs []string
		for _, p := range left {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				dirs = append(dirs, p)
			}
		}
		if len(dirs) != 0 {
			t.Fatalf("leaked cgroups: %v", dirs)
		}
	})
}
