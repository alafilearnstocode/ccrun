package run

import (
	"os"
	"os/exec"
)

func ExecPassthrough(cmd string, args []string, env []string) (int, error) {
	c := exec.Command(cmd, args...)
	c.Env = env
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if env != nil {
		c.Env = env
	}
	if err := c.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, err
	}
	return 0, nil
}
