# ccrun: Minimal Container Runtime

`ccrun` is a lightweight container runtime written in Go.  
It demonstrates Linux namespaces, cgroups, and Docker Registry integration from scratch — without relying on Docker or containerd.  

## Demo

See [docs](./docs) for usage examples and screenshots of ccrun in action.

```console
$ sudo ./ccrun run --mem 64 alpine -- sh -c 'head -c 200m /dev/zero | tail >/dev/null'
ccrun: container OOM-killed (memory limit 64 MB)
$ echo $?
137
```

## Features
- Create isolated processes using **PID, mount, UTS, and user namespaces**. ccrun re-executes itself as the container's init process to set them up.
- **Rootless**: the user namespace maps the invoking user to root inside the container, so `ccrun run alpine -- sh` works without sudo (cgroup limits need root).
- `pivot_root` into the image rootfs and detach the host filesystem (not `chroot`)
- **CPU and memory limits** with cgroups v2. The cgroup is created on the host side and the container is cloned straight into it (`CLONE_INTO_CGROUP`), so limits apply from its first instruction.
- Pull images from **Docker Hub** with a Docker Registry HTTP API v2 client
- Apply layers safely: sha256 digests are verified *before* extraction, whiteouts and opaque whiteouts are honored, and symlinks in a layer can't redirect writes outside the rootfs

## Getting Started

### Prerequisites
- Linux with cgroups v2 enabled (kernel 5.7+)
- Go 1.22+
- Root for `--mem` / `--cpu`; everything else runs rootless

### Build
```bash
go build ./cmd/ccrun
```

### Pull an Image
```bash
./ccrun pull alpine:latest
```

### Run a Container
```bash
./ccrun run --hostname box --mem 128 --cpu 50 alpine -- sh
```

### Tests
```bash
go test ./...                                                    # unit tests (layer extraction)
sudo -E env "PATH=$PATH" CCRUN_INTEGRATION=1 go test ./cmd/ccrun -v  # real containers + memory limit
```
