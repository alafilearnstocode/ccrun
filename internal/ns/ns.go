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
	MemBytes int64
	CPUPct   int
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

		sp.Credential = &syscall.Credential{Uid: 0, Gid: 0}
	}

	// The cgroup is created here, on the host side, and the child is cloned
	// directly into it (clone3 CLONE_INTO_CGROUP). Doing it in the child
	// would be too late: after pivot_root, /sys/fs/cgroup is the image's
	// directory, not the host's cgroup hierarchy.
	var cg *cgroup.Cgroup
	if cfg.MemBytes > 0 || cfg.CPUPct > 0 {
		cg, err = cgroup.New(cfg.MemBytes, cfg.CPUPct)
		if err != nil {
			return 1, fmt.Errorf("cgroup: %w", err)
		}
		sp.UseCgroupFD = true
		sp.CgroupFD = cg.FD()
	}
	cmd.SysProcAttr = sp

	code, err := run.ExitCode(cmd.Run())

	if cg != nil {
		if cg.OOMKilled() {
			fmt.Fprintf(os.Stderr, "ccrun: container OOM-killed (memory limit %d MB)\n", cfg.MemBytes/1024/1024)
		}
		if rerr := cg.Remove(); rerr != nil {
			fmt.Fprintln(os.Stderr, "ccrun: warning:", rerr)
		}
	}
	return code, err
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
	var workdir string
	var envs arrayFlags

	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")
	f.StringVar(&root, "rootfs", "", "path to root filesystem to pivot into")
	f.BoolVar(&usePID, "pidns", false, "use PID namespace (isolate process IDs)")
	f.BoolVar(&useMNT, "mntns", false, "use mount namespace (private mounts)")
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

	switch {
	case root != "":
		if !useMNT {
			fmt.Fprintln(os.Stderr, "child: -rootfs requires a mount namespace")
			os.Exit(1)
		}
		if err := rootfs.Enter(root, usePID); err != nil {
			fmt.Fprintln(os.Stderr, "rootfs:", err)
			os.Exit(1)
		}
	case useMNT:
		if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
			fmt.Fprintln(os.Stderr, "mount private /:", err)
			os.Exit(1)
		}
		// Only remount /proc when we have our own mount namespace;
		// otherwise this would cover the host's /proc.
		if usePID {
			if err := unix.Mount("proc", "/proc", "proc", 0, ""); err != nil {
				fmt.Fprintln(os.Stderr, "mount /proc:", err)
				os.Exit(1)
			}
		}
	}

	if workdir != "" {
		if err := os.Chdir(workdir); err != nil {
			fmt.Fprintln(os.Stderr, "chdir:", err)
			os.Exit(1)
		}
	}

	env := os.Environ()
	env = append(env, envs...)

	code, err := run.ExecPassthrough(target, targs, env)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
	}
	os.Exit(code)
}
