# ccrun — Minimal Container Runtime

`ccrun` is a lightweight container runtime written in Go.  
It demonstrates Linux namespaces, cgroups, and Docker Registry integration from scratch — without relying on Docker or containerd.  

## Features
- Create isolated processes using **PID, mount, UTS, and user namespaces**  
- Apply **CPU and memory limits** with cgroups v2  
- Pull container images directly from **Docker Hub** via the Registry HTTP API  
- Extract and layer images into a runnable root filesystem  
- Run minimal containers with commands like `busybox` or `alpine`

## Getting Started

### Prerequisites
- Linux with cgroups v2 enabled  
- Go 1.20+  
- Root privileges for namespace + cgroup operations  

### Build
```bash
go build ./cmd/ccrun
```

### Pull an Image
``` bash
./ccrun pull alpine:latest
```

## Demo

See [docs](./docs) for usage examples and screenshots of ccrun in action.
