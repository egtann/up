# Up

Up is a single command to get your servers are up-and-running. You can think of
`up` as a partial replacement for Kubernetes, Nomad, Docker Swarm, and other
deployment tools. Unlike those other tools, `up` is extremely small, simple,
and as a result, more reliable and less prone to bugs.

### Install

```
$ go get -u git.sr.ht/~egtann/up/cmd/up
```

### Usage

Up extracts the logic of deployment best-practices into a cross-platform tool
that can be used to deploy anything.

You'll describe your server architecture in a single file (`Upfile`), then
use the `up` command to bring everything online. The syntax of the Upfile is
deliberately similar to Makefiles.

Each Upfile contains one or more commands. All commands run locally, so remote
commands can be executed using something like `ssh user@$server "echo 'hi'"`

Variable substitution exists, and variables are identified by a `$`. Variables
can represent a single thing, such as `$remote` representing `my_user@$server`
or they can represent a series of commands, such as `$provision` representing
10 different commands to run. You'll define these commands yourself.

Up gives you access to a reserved, always-available variable in your commands:
`$server` represents the IP address in the inventory that `up` is currently
executing commands on.

You can also use environment variables, like the following:

```bash
USER=dev up -c deploy -t production
```

Access that variable in your Upfile using `$USER`.

Running commands on the remote host is as simple as using whatever shell you've
configured for your local system. See the below example Upfile designed for
bash, which runs remote commands using ssh:

```bash
# deploy is a command. Everything that follows on this line, similar to Make,
# is a requirement. In this example, running `up deploy` will first run
# check_health and check_version. If check_health or check_version fail (return
# a non-zero status code), then the commands are run. If both succeed, deploy
# is skipped on this server.
deploy check_health check_version
	# your steps to compile and copy files to the remote server go here.
	# If any of the following lines have non-zero exits, up immediately
	# exits with status code 1.
	go build -o myserver git.sr.ht/example/myserver
	rsync -chazP myserver $remote:
	rm myserver
	ssh $remote 'sudo service myserver restart'
	sleep 10 && $check_health

update
	ssh $remote 'sudo apt -y update && sudo apt -y upgrade'
	ssh $remote 'sudo snap refresh'

check_health
	curl -s --max-time 1 $server/health

check_version
	expr $CHECKSUM == `curl --max-time 1 $server/version`

remote
	$UP_USER@$server
```

An **inventory.json** file must also be defined:

```json
{
	"10.0.0.1": ["production", "debian"],
	"10.0.0.2": ["production", "debian"],
	"10.0.0.3": ["staging", "debian"]
}
```

Using the example Upfile above, here's how we could deploy to staging:

```
up -c deploy -t staging
```

Since `up` does these tasks by running arbitrary shell commands defined in your
project-level `Upfile`, `up` works out-of-the-box with:

* All cloud providers
* Ansible
* Containers (Docker, rkt, LXC, etc.)
* Bash scripts
* And any other tools with command-line interfaces

If we want to deploy to staging and production, we'd write:

```
up -c deploy -t staging,production
```

To update all of our debian servers, 2 at a time, and exit immediately if any
fail, we can run:

```
up -c update -t debian -n 2
```

Run `up -h` for additional usage info.

### Features

- [x] Define your system architecture in source control
- [x] Run arbitrary shell commands to provision, start, and check the health of
      your servers
- [x] Operate on individual environments, like production and staging
- [x] Rolling, concurrent, agentless deploys
- [x] Stateless. Up checks your infrastructure to determine its state on each
      run, so nothing is ever out-of-date

### Non-Features

Like any good UNIX tool, `up` aims to do one thing and do it well. The
following features are out of the scope of `up`:

* Bin-packing
* Logging
* On-going monitoring
* Restarting apps after crashes
* Spinning up new servers via cloud providers
* Scaling servers up or down with demand
