package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/alafilearnstocode/ccrun/internal/ns"
	"github.com/alafilearnstocode/ccrun/internal/run"
)

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "__ccrun_child__":
		ns.ChildMain()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage: ccrun run [ -hostname NAME ] [ -rootfs PATH ] [ -pidns ] [ -mntns ] -- <command> [args...]")
	os.Exit(2)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	hostname := fs.String("hostname", "", "UTS hostname inside container")
	root := fs.String("rootfs", "", "Path to container rootfs for chroot")
	pidns := fs.Bool("pidns", false, "Use a new PID namespace")
	mntns := fs.Bool("mntns", false, "Use a new mount namespace (private mounts)")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		log.Fatal("no command provided")
	}

	if *hostname == "" && *root == "" && !*pidns && !*mntns {
		code, err := run.ExecPassthrough(rest[0], rest[1:], os.Environ())
		if err != nil && code == 0 {
			code = 1
		}
		os.Exit(code)
	}

	cfg := ns.Config{
		Hostname: *hostname,
		UseUTS:   *hostname != "",
		Rootfs:   *root,
		UsePID:   *pidns,
		UseMNT:   *mntns,
	}
	code, err := ns.SpawnChild(cfg, rest[0], rest[1:])
	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}
