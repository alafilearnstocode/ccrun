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
	UsePID   bool // PID namespace
	UseMNT   bool // Mount namespace
	UseUSER  bool // User namespace (rootless)
}

// SpawnChild re-execs this binary with requested namespaces and arguments.
// Child flags are placed before "--"; the target command and its args follow after.
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
	if cfg.UseUSER {
		argv = append(argv, "-userns")
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
	if cfg.UseUSER {
		sp.Cloneflags |= unix.CLONE_NEWUSER

		uid := os.Getuid()
		gid := os.Getgid()
		sp.UidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: uid, Size: 1},
		}
		// setgroups must be disabled before writing gid_map in userns
		sp.GidMappingsEnableSetgroups = false
		sp.GidMappings = []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: gid, Size: 1},
		}
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

// ChildMain executes in the child process after namespaces are created.
func ChildMain() {
	f := flag.NewFlagSet(childSub, flag.ExitOnError)
	var useUTS bool
	var hostname string
	var root string
	var usePID bool
	var useMNT bool
	var useUSER bool

	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")
	f.StringVar(&root, "rootfs", "", "path to root filesystem to chroot into")
	f.BoolVar(&usePID, "pidns", false, "use PID namespace (isolate process IDs)")
	f.BoolVar(&useMNT, "mntns", false, "use mount namespace (private mounts)")
	f.BoolVar(&useUSER, "userns", false, "use user namespace (rootless)")

	f.Parse(os.Args[2:])
	rest := f.Args() // after Parse, "--" is removed; remainder is <cmd> <args...>
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "child: missing <cmd>")
		os.Exit(2)
	}
	target := rest[0]
	targs := rest[1:]

	// Configure UTS (hostname)
	if useUTS && hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintln(os.Stderr, "sethostname:", err)
			os.Exit(1)
		}
	}

	// Enter chroot if requested
	if root != "" {
		if err := rootfs.EnterChroot(root); err != nil {
			fmt.Fprintln(os.Stderr, "chroot:", err)
			os.Exit(1)
		}
	}

	// If using a mount namespace, privatize mounts so /proc doesn't leak to host
	if useMNT {
		// On some kernels, setting MS_PRIVATE directly on / fails unless it's first a bind mount.
		// Make / a recursive bind mount of itself, then mark it private recursively.
		if err := unix.Mount("/", "/", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			fmt.Fprintln(os.Stderr, "bind mount /:", err)
			os.Exit(1)
		}
		if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount private /:", err)
			os.Exit(1)
		}
	}

	// If PID namespace is active, mount a fresh /proc for the container
	cleanupProc := false
	if usePID {
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount /proc:", err)
			os.Exit(1)
		}
		cleanupProc = true
	}

	// Execute target command
	code, err := run.ExecPassthrough(target, targs, os.Environ())

	// Cleanup mounts created by the child
	if cleanupProc {
		_ = unix.Unmount("/proc", 0)
	}

	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}
