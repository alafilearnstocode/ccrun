package ns

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"github.com/alafilearnstocode/ccrun/internal/run"
	"golang.org/x/sys/unix"
)

const childSub = "__ccrun_child__"

type Config struct {
	Hostname string
	UseUTS   bool
}

func SpawnChild(cfg Config, command string, args []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}

	argv := []string{childSub}
	if cfg.UseUTS {
		argv = append(argv, "--uts", "--hostname", cfg.Hostname)
	}
	argv = append(argv, "--", command)
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

func ChildMain() {
	f := flag.NewFlagSet(childSub, flag.ExitOnError)
	var useUTS bool
	var hostname string
	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")
	f.Parse(os.Args[2:])
	args := f.Args()

	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep == -1 || sep == len(args)-1 {
		fmt.Fprintln(os.Stderr, "child: missing -- <cmd>")
		os.Exit(2)
	}
	target := args[sep+1]
	targs := args[sep+2:]

	if useUTS && hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintln(os.Stderr, "sethostname:", err)
			os.Exit(1)
		}
	}
	code, err := run.ExecPassthrough(target, targs, os.Environ())
	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}
