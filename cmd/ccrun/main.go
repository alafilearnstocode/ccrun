package main

import (
	"fmt"
	"log"
	"os"

	"github.com/alafilearnstocode/ccrun/internal/run"
)

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: ccrun run <command> [args...]")
	os.Exit(2)
}

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 || os.Args[1] != "run" {
		usage()
	}
	args := os.Args[2:]
	if len(args) == 0 {
		log.Fatal("no command provided")
	}
	exit, err := run.ExecPassthrough(args[0], args[1:], os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "ccrun:", err)
		if exit == 0 {
			exit = 1
		}
	}
	os.Exit(exit)
}
