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
	fmt.Fprintln(os.Stderr, "Usage: ccrun run [ -hostname NAME ] [ -rootfs PATH ] [ -pidns ] [ -mntns ] [ -userns ] [ -mem MB ] [ -cpu PCT ] -- <command> [args...]")
	fmt.Fprintln(os.Stderr, "       ccrun pull [ --out DIR ] <image[:tag]>")
	os.Exit(2)
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	hostname := fs.String("hostname", "", "UTS hostname inside container")
	root := fs.String("rootfs", "", "Path to container rootfs for chroot")
	pidns := fs.Bool("pidns", false, "Use a new PID namespace")
	mntns := fs.Bool("mntns", false, "Use a new mount namespace (private mounts)")
	userns := fs.Bool("userns", false, "Use a new user namespace (rootless)")
	mem := fs.Int("mem", 0, "Memory limit in MB (0 = unlimited)")
	cpu := fs.Int("cpu", 0, "CPU limit in percent (0 or >=100 = unlimited)")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) == 0 {
		log.Fatal("no command provided")
	}

	if *hostname == "" && *root == "" && !*pidns && !*mntns && !*userns && *mem == 0 && *cpu == 0 {
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
		MemBytes: int64(*mem) * 1024 * 1024,
		CPUPct:   *cpu,
	}
	code, err := ns.SpawnChild(cfg, rest[0], rest[1:])
	if err != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}

func pullCmd(args []string) {
	fs := flag.NewFlagSet("pull", flag.ExitOnError)
	outDir := fs.String("out", "images", "Output images directory")
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
