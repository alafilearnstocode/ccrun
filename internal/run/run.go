package run

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

func ExecPassthrough(cmd string, args []string, env []string) (int, error) {
	c := exec.Command(cmd, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if env != nil {
		c.Env = env
	}
	return ExitCode(c.Run())
}

// ExitCode converts the error from exec.Cmd.Run into a shell-style exit
// code. A process killed by a signal reports 128+signal (e.g. 137 for
// SIGKILL from the OOM killer) instead of exec's -1.
func ExitCode(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return 1, err
}
