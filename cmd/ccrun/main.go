package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/alafilearnstocode/ccrun/internal/ns"
	"github.com/alafilearnstocode/ccrun/internal/registry"
	"github.com/alafilearnstocode/ccrun/internal/run"
)

// repeatable --env flags
type arrayFlags []string

func (i *arrayFlags) String() string         { return fmt.Sprint(*i) }
func (i *arrayFlags) Set(value string) error { *i = append(*i, value); return nil }

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
	}

	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "pull":
		pullCmd(os.Args[2:])
	case "__ccrun_child__":
		ns.ChildMain()
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr,
		"Usage:\n"+
			"  ccrun run [--hostname NAME] [--rootfs PATH] [--pidns] [--mntns] [--userns] [--mem MB] [--cpu PCT] [--workdir DIR] [--env K=V] -- <command> [args...]\n"+
			"  ccrun pull [--out DIR] <image[:tag]>",
	)
	os.Exit(2)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)

	hostname := fs.String("hostname", "", "UTS hostname inside container")
	root := fs.String("rootfs", "", "path to container rootfs (chroot)")
	pidns := fs.Bool("pidns", false, "use new PID namespace")
	mntns := fs.Bool("mntns", false, "use new mount namespace")
	userns := fs.Bool("userns", false, "use new user namespace (rootless)")
	memMB := fs.Int64("mem", 0, "memory limit in MB (0 = unlimited)")
	cpuPct := fs.Int("cpu", 0, "CPU limit in percent (0 or >=100 = unlimited)")
	workdir := fs.String("workdir", "", "working directory inside container")
	var envs arrayFlags
	fs.Var(&envs, "env", "environment variable KEY=VAL (repeatable)")

	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		log.Fatal("no command provided")
	}

	// Fast path: Step-1 behavior when no isolation/limits/overrides in use
	if *hostname == "" && *root == "" && !*pidns && !*mntns && !*userns && *memMB == 0 && *cpuPct == 0 && *workdir == "" && len(envs) == 0 {
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
		UseUSER:  *userns,
		MemBytes: *memMB * 1024 * 1024,
		CPUPct:   *cpuPct,
		Workdir:  *workdir,
		Env:      envs,
	}
	code, err := ns.SpawnChild(cfg, rest[0], rest[1:])
	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}

func pullCmd(args []string) {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	outDir := fs.String("out", "images", "output images directory")
	fs.Parse(args)

	if fs.NArg() != 1 {
		log.Fatal("usage: ccrun pull [--out DIR] <image[:tag]>")
	}

	ref, err := registry.ParseImageRef(fs.Arg(0))
	if err != nil {
		log.Fatal(err)
	}

	dest := filepath.Join(*outDir, ref.RepoPath(), ref.Tag)
	if err := registry.Pull(ref, dest); err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Pulled %s to %s\n", ref.String(), dest)
}
