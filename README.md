# Up

Up is a single command to get your servers are up-and-running. You can think of
`up` as a partial replacement for Kubernetes, Nomad, Docker Swarm, and other
deployment tools. Unlike those other tools, `up` is extremely small, simple,
and as a result, more reliable and less prone to bugs.

### Install

```
$ go get github.com/egtann/up
```

### Usage

You'll describe your server architecture in a single file (`Upfile.toml`), then
use the `up` command to bring everything online. You can find an example
`Upfile.toml` in this project.

Running `up` performs 3 tasks on each server:

* Provision (once per IP/service combination)
* Start
* Check health

Since `up` does these tasks by running arbitrary shell commands defined in your
project-level `Upfile.toml` file, `up` works out-of-the-box with:

* All cloud providers
* Ansible
* Containers (Docker, rkt, LXC, etc.)
* Bash scripts
* And any other tools where you have IP-level control over your servers

The only required flag for `up` is `-e`, which specifies the environment. Run
`up -h` for additional usage info.

Provisioning will only run once per IP. To force re-provisioning, delete the
server's IP address from the generated `Upfile.lock` file.

### Features

- [x] Define your system architecture in source control
- [x] Run arbitrary shell commands to provision, start, and check the health of
      your servers
- [x] Operate on individual environments, like production and staging
- [x] Perform dry runs to check commands prior to running
- [ ] Concurrent deploys
- [ ] Rolling and blue-green deploys
- [ ] Pass in template variables via the `up` CLI

### Non-Features

Like any good UNIX tool, `up` aims to do one thing and do it well. The
following features are out of the scope of `up`:

* Bin-packing
* Logging
* On-going monitoring
* Restarting apps after crashes
* Spinning up new servers via cloud providers
* Scaling servers up or down with demand
