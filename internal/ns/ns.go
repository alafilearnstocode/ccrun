package ns

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/alafilearnstocode/ccrun/internal/rootfs"
	"github.com/alafilearnstocode/ccrun/internal/run"
	"golang.org/x/sys/unix"
)

const childSub = "__ccrun_child__"

type Config struct {
	Hostname string
	UseUTS   bool
	Rootfs   string
}

// re-exec self with clone flags; pass child flags before "--", target after
func SpawnChild(cfg Config, command string, args []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}

	argv := []string{childSub}
	if cfg.UseUTS {
		argv = append(argv, "-uts", "-hostname", cfg.Hostname)
	}
	if cfg.Rootfs != "" {
		argv = append(argv, "-rootfs", cfg.Rootfs)
	}
	argv = append(argv, "--")
	argv = append(argv, command)
	argv = append(argv, args...)

	cmd := exec.Command(self, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sp := &syscall.SysProcAttr{}
	if cfg.UseUTS {
		sp.Cloneflags |= unix.CLONE_NEWUTS
	}
	cmd.SysProcAttr = sp

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// runs inside child; parse flags, set hostname, optionally chroot, then exec target
func ChildMain() {
	f := flag.NewFlagSet(childSub, flag.ExitOnError)
	var useUTS bool
	var hostname string
	var root string
	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")
	f.StringVar(&root, "rootfs", "", "path to root filesystem to chroot into")

	f.Parse(os.Args[2:])
	rest := f.Args() // after Parse, "--" is removed; rest == <cmd> <args...>

	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "child: missing <cmd>")
		os.Exit(2)
	}
	target := rest[0]
	targs := rest[1:]

	if useUTS && hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintln(os.Stderr, "sethostname:", err)
			os.Exit(1)
		}
	}
	if root != "" {
		if err := rootfs.EnterChroot(root); err != nil {
			fmt.Fprintln(os.Stderr, "chroot:", err)
			os.Exit(1)
		}
	}

	code, err := run.ExecPassthrough(target, targs, os.Environ())
	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}

package ns

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/alafilearnstocode/ccrun/internal/rootfs"
	"github.com/alafilearnstocode/ccrun/internal/run"
	"golang.org/x/sys/unix"
)

const childSub = "__ccrun_child__"

type Config struct {
	Hostname string
	UseUTS   bool
	Rootfs   string
	UsePID   bool // NEW: PID namespace
	UseMNT   bool // NEW: Mount namespace
}

// re-exec self with clone flags; pass child flags before "--", target after
func SpawnChild(cfg Config, command string, args []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}

	argv := []string{childSub}
	if cfg.UseUTS {
		argv = append(argv, "-uts", "-hostname", cfg.Hostname)
	}
	if cfg.Rootfs != "" {
		argv = append(argv, "-rootfs", cfg.Rootfs)
	}
	if cfg.UsePID {
		argv = append(argv, "-pidns")
	}
	if cfg.UseMNT {
		argv = append(argv, "-mntns")
	}
	argv = append(argv, "--")
	argv = append(argv, command)
	argv = append(argv, args...)

	cmd := exec.Command(self, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	sp := &syscall.SysProcAttr{}
	if cfg.UseUTS {
		sp.Cloneflags |= unix.CLONE_NEWUTS
	}
	if cfg.UsePID {
		sp.Cloneflags |= unix.CLONE_NEWPID
	}
	if cfg.UseMNT {
		sp.Cloneflags |= unix.CLONE_NEWNS
	}
	cmd.SysProcAttr = sp

	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}

// runs inside child; parse flags, set hostname, chroot, make mounts private, mount /proc, then exec target
func ChildMain() {
	f := flag.NewFlagSet(childSub, flag.ExitOnError)
	var useUTS bool
	var hostname string
	var root string
	var usePID bool
	var useMNT bool
	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")
	f.StringVar(&root, "rootfs", "", "path to root filesystem to chroot into")
	f.BoolVar(&usePID, "pidns", false, "use PID namespace (isolate process IDs)")
	f.BoolVar(&useMNT, "mntns", false, "use mount namespace (private mounts)")

	f.Parse(os.Args[2:])
	rest := f.Args() // after Parse, "--" is removed; rest == <cmd> <args...>
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "child: missing <cmd>")
		os.Exit(2)
	}
	target := rest[0]
	targs := rest[1:]

	// Set hostname if requested
	if useUTS && hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintln(os.Stderr, "sethostname:", err)
			os.Exit(1)
		}
	}

	// Enter chroot if requested (Step 3)
	if root != "" {
		if err := rootfs.EnterChroot(root); err != nil {
			fmt.Fprintln(os.Stderr, "chroot:", err)
			os.Exit(1)
		}
	}

	// If using mount namespace, make mounts private so /proc doesn't leak
	if useMNT {
		if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount private /:", err)
			os.Exit(1)
		}
	}

	// If using PID namespace, mount /proc inside the container
	cleanupProc := false
	if usePID {
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount /proc:", err)
			os.Exit(1)
		}
		cleanupProc = true
	}

	// Run target
	code, err := run.ExecPassthrough(target, targs, os.Environ())

	// Cleanup
	if cleanupProc {
		_ = unix.Unmount("/proc", 0)
	}

	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}