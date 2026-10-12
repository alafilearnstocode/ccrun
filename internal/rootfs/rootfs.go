package rootfs

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const oldRootName = ".pivot_old"

// Enter makes dir the root filesystem of the calling process using
// pivot_root(2). It must run inside a new mount namespace.
//
// pivot_root is used instead of chroot because chroot only changes the
// process's view of "/": the old root stays mounted and reachable (e.g. via
// a second chroot with an open fd). pivot_root swaps the root mount itself,
// and after the old root is detached there is nothing left to escape to.
func Enter(dir string, mountProc bool) error {
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}

	// Stop our mounts from propagating back to the host namespace. This has
	// to happen before any other mount, not after.
	if err := unix.Mount("", "/", "", unix.MS_PRIVATE|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("make / private: %w", err)
	}

	// pivot_root requires new_root to be a mount point.
	if err := unix.Mount(root, root, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return fmt.Errorf("bind mount rootfs: %w", err)
	}

	// Mount /proc before pivoting: inside a user namespace the kernel only
	// allows a new proc mount while a fully visible proc is still mounted
	// in this namespace, and that goes away when the old root is detached.
	if mountProc {
		p := filepath.Join(root, "proc")
		if err := os.MkdirAll(p, 0o555); err != nil {
			return err
		}
		if err := unix.Mount("proc", p, "proc", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
			return fmt.Errorf("mount /proc: %w", err)
		}
	}

	if err := mountDev(root); err != nil {
		return err
	}

	oldRoot := filepath.Join(root, oldRootName)
	if err := os.MkdirAll(oldRoot, 0o700); err != nil {
		return err
	}
	if err := unix.PivotRoot(root, oldRoot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := unix.Chdir("/"); err != nil {
		return err
	}

	// Detach the host's filesystem so nothing in the container can reach it.
	if err := unix.Unmount("/"+oldRootName, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount old root: %w", err)
	}
	return os.Remove("/" + oldRootName)
}

// mountDev gives the container a minimal /dev. Image layers can't carry
// device nodes we could create (mknod needs real root), so the standard
// ones are bind-mounted from the host, the same way rootless runc does it.
func mountDev(root string) error {
	dev := filepath.Join(root, "dev")
	if err := os.MkdirAll(dev, 0o755); err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", dev, "tmpfs", unix.MS_NOSUID|unix.MS_STRICTATIME, "mode=755"); err != nil {
		return fmt.Errorf("mount /dev: %w", err)
	}
	for _, name := range []string{"null", "zero", "full", "random", "urandom", "tty"} {
		target := filepath.Join(dev, name)
		f, err := os.Create(target)
		if err != nil {
			return err
		}
		f.Close()
		if err := unix.Mount("/dev/"+name, target, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind /dev/%s: %w", name, err)
		}
	}
	return nil
}
