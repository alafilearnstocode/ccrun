package ns

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/alafilearnstocode/ccrun/internal/cgroup"
	"github.com/alafilearnstocode/ccrun/internal/rootfs"
	"github.com/alafilearnstocode/ccrun/internal/run"
	"golang.org/x/sys/unix"
)

const childSub = "__ccrun_child__"

type Config struct {
	Hostname string
	UseUTS   bool
	Rootfs   string
	UsePID   bool
	UseMNT   bool
	UseUSER  bool
	MemBytes int64 // memory limit in bytes (0 = unlimited)
	CPUPct   int   // CPU percent (0 or >=100 = unlimited)
	Workdir  string
	Env      []string
}

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
	if cfg.MemBytes > 0 {
		argv = append(argv, "-mem", fmt.Sprintf("%d", cfg.MemBytes/1024/1024))
	}
	if cfg.CPUPct > 0 {
		argv = append(argv, "-cpu", fmt.Sprintf("%d", cfg.CPUPct))
	}
	if cfg.Workdir != "" {
		argv = append(argv, "-workdir", cfg.Workdir)
	}
	for _, e := range cfg.Env {
		argv = append(argv, "-env", e)
	}
	argv = append(argv, "--", command)
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

		sp.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: uid, Size: 1}}
		sp.GidMappingsEnableSetgroups = false
		sp.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: gid, Size: 1}}

		// Be root inside the new user namespace
		sp.Credential = &syscall.Credential{Uid: 0, Gid: 0}
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

type arrayFlags []string

func (i *arrayFlags) String() string {
	return fmt.Sprint(*i)
}

func (i *arrayFlags) Set(value string) error {
	*i = append(*i, value)
	return nil
}

func ChildMain() {
	f := flag.NewFlagSet(childSub, flag.ExitOnError)
	var useUTS bool
	var hostname string
	var root string
	var usePID bool
	var useMNT bool
	var useUSER bool
	var memMB int
	var cpuPct int
	var workdir string
	var envs arrayFlags

	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")
	f.StringVar(&root, "rootfs", "", "path to root filesystem to chroot into")
	f.BoolVar(&usePID, "pidns", false, "use PID namespace (isolate process IDs)")
	f.BoolVar(&useMNT, "mntns", false, "use mount namespace (private mounts)")
	f.BoolVar(&useUSER, "userns", false, "use user namespace (rootless)")
	f.IntVar(&memMB, "mem", 0, "memory limit in MB (0 = unlimited)")
	f.IntVar(&cpuPct, "cpu", 0, "CPU limit in percent (0 or >=100 = unlimited)")
	f.StringVar(&workdir, "workdir", "", "working directory inside container")
	f.Var(&envs, "env", "environment variable KEY=VAL (repeatable)")

	f.Parse(os.Args[2:])
	rest := f.Args()
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

	if workdir != "" {
		if err := os.Chdir(workdir); err != nil {
			fmt.Fprintln(os.Stderr, "chdir:", err)
			os.Exit(1)
		}
	}

	if useMNT {
		if err := unix.Mount("/", "/", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			fmt.Fprintln(os.Stderr, "bind mount /:", err)
			os.Exit(1)
		}
		if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount private /:", err)
			os.Exit(1)
		}
	}

	cleanupProc := false
	if usePID {
		if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount /proc:", err)
			os.Exit(1)
		}
		cleanupProc = true
	}

	// cgroups v2 limits
	var cgPath string
	if memMB > 0 || cpuPct > 0 {
		memBytes := int64(memMB) * 1024 * 1024
		p, err := cgroup.SetupAndEnter(memBytes, cpuPct)
		if err != nil {
			fmt.Fprintln(os.Stderr, "cgroup:", err)
			os.Exit(1)
		}
		cgPath = p
	}

	env := os.Environ()
	env = append(env, envs...)

	code, err := run.ExecPassthrough(target, targs, env)

	if cleanupProc {
		_ = unix.Unmount("/proc", 0)
	}
	if cgPath != "" {
		cgroup.Cleanup(cgPath)
	}

	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}
