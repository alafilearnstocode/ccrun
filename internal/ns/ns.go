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

// SpawnChild re-execs the current binary with namespace flags.
// We pass our child flags (-uts/-hostname) BEFORE the "--",
// and the target program AFTER the "--".
func SpawnChild(cfg Config, command string, args []string) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 1, err
	}

	argv := []string{childSub}
	if cfg.UseUTS {
		argv = append(argv, "-uts", "-hostname", cfg.Hostname)
	}
	argv = append(argv, "--") // separator between child flags and target command
	argv = append(argv, command)
	argv = append(argv, args...)

	cmd := exec.Command(self, argv...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr

	// SysProcAttr defines clone/namespace behavior
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

// ChildMain runs inside the namespaced child. It parses child flags (-uts/-hostname),
// then looks for the "--" separator and executes the target program.
func ChildMain() {
	f := flag.NewFlagSet(childSub, flag.ExitOnError)
	var useUTS bool
	var hostname string
	f.BoolVar(&useUTS, "uts", false, "use UTS namespace")
	f.StringVar(&hostname, "hostname", "", "hostname inside container")

	// Parse child flags from args after the program name and subcommand.
	// For argv: [ccrun __ccrun_child__ <flags> -- <cmd> <args>...]
	f.Parse(os.Args[2:])
	rest := f.Args()

	// After f.Parse, the standard flag package has already consumed and removed
	// the `"--"` separator. The remaining args are: <cmd> <args...>.
	if len(rest) == 0 {
		fmt.Fprintln(os.Stderr, "child: missing <cmd>")
		os.Exit(2)
	}
	target := rest[0]
	targs := rest[1:]

	// Set the hostname if requested
	if useUTS && hostname != "" {
		if err := unix.Sethostname([]byte(hostname)); err != nil {
			fmt.Fprintln(os.Stderr, "sethostname:", err)
			os.Exit(1)
		}
	}

	// Exec the target with inherited stdio and env
	code, err := run.ExecPassthrough(target, targs, os.Environ())
	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}
