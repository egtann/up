// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/egtann/up"
)

type flags struct {
	// Upfile allows you to specify a different Upfile name. This is
	// helpful when running across multiple operating systems or shells.
	// For example, you may have Upfile.windows.toml and Upfile.linux.toml,
	// or Upfile.bash.toml and Upfile.fish.toml.
	Upfile string

	// Inventory allows you specify a different inventory name.
	Inventory string

	// Command to run. Like `make`, an empty Command defaults to the first
	// command in the Upfile.
	Command up.CmdName

	// Tags limits the changed services to those enumerated if the flag is
	// provided. This holds the tags that will be used.
	Tags map[string]struct{}

	// Serial determines how many servers of the same type will be operated
	// on at any one time. This defaults to 1. Use 0 to specify all of
	// them.
	Serial int

	// Directory used to calculate the checksum. Defaults to the current
	// directory.
	Directory string

	// Vars passed into `up` at runtime to be used in start commands.
	Vars map[string]string

	// Stdin instructs `up` to read from stdin, achieved with `up -`.
	Stdin bool

	// Verbose will log full commands, even when they're very log. By
	// default `up` truncates commands to 80 characters when logging,
	// except in the case of a failure where the full command is displayed.
	Verbose bool

	// Prompt instructs `up` to wait for input before moving onto the next
	// batch.
	Prompt bool
}

type batch map[string][][]string

type result struct {
	server string
	err    error
}

func main() {
	log.SetFlags(0)
	rand.Seed(time.Now().UnixNano())

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	log.Println("success")
}

func run() error {
	flgs, err := parseFlags()
	if err != nil {
		return usage(fmt.Errorf("parse flags: %w", err))
	}

	var upFi io.ReadCloser
	if flgs.Stdin {
		upFi = os.Stdin
	} else {
		upFi, err = os.Open(flgs.Upfile)
		if err != nil {
			return fmt.Errorf("open upfile: %w", err)
		}
		defer upFi.Close()
	}
	conf, err := up.ParseUpfile(upFi)
	if err != nil {
		return fmt.Errorf("parse upfile: %w", err)
	}

	// Open and parse the inventory file
	invFi, err := os.Open(flgs.Inventory)
	if err != nil {
		return fmt.Errorf("open inventory: %w", err)
	}
	defer invFi.Close()
	inventory, err := up.ParseInventory(invFi)
	if err != nil {
		return fmt.Errorf("parse inventory: %w", err)
	}

	if flgs.Command != "" && flgs.Upfile != "-" {
		conf.DefaultCommand = flgs.Command
		if _, exist := conf.Commands[conf.DefaultCommand]; !exist {
			return fmt.Errorf("undefined command: %s", conf.DefaultCommand)
		}
	}
	lims := []string{}
	for lim := range flgs.Tags {
		lims = append(lims, string(lim))
	}
	tmp := strings.Join(lims, ", ")
	if tmp == "" {
		tmp = string(conf.DefaultCommand)
	}

	if _, exist := inventory["all"]; exist {
		return errors.New("reserved keyword 'all' cannot be inventory name")
	}

	// Default the tags equal to the command name, which makes the
	// following work: `upgen my_app | up -`
	if len(flgs.Tags) == 0 {
		flgs.Tags[string(conf.DefaultCommand)] = struct{}{}
	}

	// Remove any unnecessary inventory. All remaining defined inventory
	// will be used.
	if _, exist := flgs.Tags["all"]; !exist {
		for ip, tags := range inventory {
			var found bool
			for _, t := range tags {
				if _, exist := flgs.Tags[t]; exist {
					found = true
					break
				}
			}
			if !found {
				delete(inventory, ip)
			}
		}
	}

	// Remove any tags which are not in the provided flags, as we'll be
	// ignoring those
	for ip, tags := range inventory {
		var newTags []string
		for _, t := range tags {
			if _, exist := flgs.Tags[t]; !exist {
				continue
			}
			newTags = append(newTags, t)
		}
		inventory[ip] = newTags
	}

	// Validate all tags are defined in inventory (i.e. no silent failure
	// on typos).
	if len(inventory) == 0 {
		msg := fmt.Sprintf("tags not defined in inventory: ")
		for l := range flgs.Tags {
			msg += fmt.Sprintf("%s, ", l)
		}
		return errors.New(strings.TrimSuffix(msg, ", "))
	}

	log.Printf("running %s on %s\n", conf.DefaultCommand, tmp)

	// Calculate a sha256 checksum on the provided directory (defaults to
	// current directory).
	log.Printf("calculating checksum\n")
	chk, err := calcChecksum(flgs.Directory)
	if err != nil {
		return fmt.Errorf("calc checksum: %w", err)
	}

	// Split into batches limited in size by the provided Serial flag.
	batches, err := makeBatches(conf, inventory, flgs.Serial)
	if err != nil {
		return fmt.Errorf("make batches: %w", err)
	}
	log.Printf("got batches: %v\n", batches)

	// Prepare our channels to synchronize waiting for user confirmation
	// before deploying each batch. Sends on the confirm channel will not
	// block because it's pre-buffered for all batches.
	confirm := make(chan struct{}, len(batches))
	if flgs.Prompt {
		// We don't want to prompt for the first command -- just for
		// every command after, so we send an initial confirmation now
		confirm <- struct{}{}
	} else {
		for i := 0; i < len(batches); i++ {
			confirm <- struct{}{}
		}
	}

	// For each batch, run the ExecIfs and run Execs if necessary.
	done := make(chan struct{}, len(batches))
	crash := make(chan error)
	defer close(crash)
	for _, srvBatch := range batches {
		<-confirm

		// We are good to go. Schedule the batch.
		go func(srvBatch [][]string) {
			for _, srvGroup := range srvBatch {
				ch := make(chan result, len(srvGroup))
				srvGroup = randomizeOrder(srvGroup)
				cmd := conf.Commands[conf.DefaultCommand]
				runExecIfs(ch, flgs.Vars, conf.Commands, cmd,
					chk, srvGroup, flgs.Verbose)
				for i := 0; i < len(srvGroup); i++ {
					res := <-ch
					if res.err != nil {
						crash <- res.err
					}
				}
			}
			done <- struct{}{}
		}(srvBatch)

		// If we're confirming the next one, prepare the prompt
		if flgs.Prompt {
			if keepGoing := confirmPrompt(confirm); !keepGoing {
				return nil
			}
		}
	}
	for i := 0; i < len(batches); i++ {
		select {
		case <-done:
			// Keep going
		case err := <-crash:
			return err
		}
	}
	return nil
}

// confirmPrompt prompts the user and asks if up should continue.
func confirmPrompt(confirm chan struct{}) bool {
	var shouldContinue string
	fmt.Printf("do you want to continue? [Y/n] ")

	rdr := bufio.NewReader(os.Stdin)
	shouldContinue, err := rdr.ReadString('\n')
	if err != nil {
		fmt.Printf("failed to read: %s\n", err)
		return false
	}
	shouldContinue = strings.TrimSuffix(shouldContinue, "\n")
	switch strings.ToLower(shouldContinue) {
	case "y", "yes", "":
		confirm <- struct{}{}
		return true
	case "n", "no":
		fmt.Println("stopping up")
		return false
	default:
		fmt.Printf("unknown input: %s\n", shouldContinue)
		return confirmPrompt(confirm)
	}
}

func runExecIfs(
	ch chan result,
	vars map[string]string,
	cmds map[up.CmdName]*up.Cmd,
	cmd *up.Cmd,
	chk string,
	servers []string,
	verbose bool,
) {
	send := func(ch chan<- result, err error, servers []string) {
		for _, srv := range servers {
			ch <- result{server: srv, err: err}
		}
	}
	var needToRun bool
	for _, execIf := range cmd.ExecIfs {
		// TODO should this also enforce ExecIfs? Probably...
		// TODO this should handle errors correctly through the channel
		cmds := copyCommands(cmds)
		steps := cmds[execIf].Execs
		for _, step := range steps {
			ok, err := runExec(vars, cmds, step, chk, servers,
				true, verbose)
			if err != nil {
				send(ch, err, servers)
				return
			}
			if !ok {
				needToRun = true
			}
		}
	}
	if !needToRun && len(cmd.ExecIfs) > 0 {
		for _, srv := range servers {
			ch <- result{server: srv}
		}
		return
	}
	for _, cmdLine := range cmd.Execs {
		cmdLine, err := substituteVariables(vars, cmds, cmdLine)
		if err != nil {
			send(ch, err, servers)
			return
		}

		// We may have substituted a variable with a multi-line command
		cmdLines := strings.SplitN(cmdLine, "\n", -1)
		for _, cmdLine := range cmdLines {
			_, err = runExec(vars, cmds, cmdLine, chk, servers,
				false, verbose)
			if err != nil {
				send(ch, err, servers)
				return
			}
		}
	}
	send(ch, nil, servers)
}

// runExec reports whether all execIfs passed and an error if any.
func runExec(
	vars map[string]string,
	cmds map[up.CmdName]*up.Cmd,
	cmd, chk string,
	servers []string,
	execIf, verbose bool,
) (bool, error) {
	cmds = copyCommands(cmds)
	cmds["checksum"] = &up.Cmd{Execs: []string{chk}}
	ch := make(chan runResult, len(servers))
	for _, server := range servers {
		go runCmd(ch, vars, cmds, cmd, chk, server, execIf, verbose)
	}
	var err error
	pass := true
	for i := 0; i < len(servers); i++ {
		res := <-ch
		pass = pass && res.pass
		if res.error != nil {
			err = res.error
		}
	}
	return pass, err
}

type runResult struct {
	pass  bool
	error error
}

func runCmd(
	ch chan<- runResult,
	vars map[string]string,
	cmds map[up.CmdName]*up.Cmd,
	cmd, chk, server string,
	execIf, verbose bool,
) {
	// TODO ensure that no cycles are present with depth-first
	// search

	// Now substitute any variables designated by a '$'
	cmds = copyCommands(cmds)
	cmds["server"] = &up.Cmd{Execs: []string{server}}
	cmd, err := substituteVariables(vars, cmds, cmd)
	if err != nil {
		err = fmt.Errorf("substitute: %w", err)
		ch <- runResult{pass: false, error: err}
		return
	}

	logLine := fmt.Sprintf("[%s] %s", server, cmd)
	if !verbose && len(logLine) > 90 {
		logLine = logLine[:87] + "..."
	}
	log.Printf("%s\n", logLine)

	c := exec.Command("sh", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	c.Stdin = os.Stdin
	if err = c.Run(); err != nil {
		if execIf {
			// TODO log if verbose
			ch <- runResult{pass: false}
			return
		}

		fmt.Println("error running command:", cmd)
		ch <- runResult{pass: false, error: err}
		return
	}
	ch <- runResult{pass: true}
}

// parseFlags and validate them.
func parseFlags() (flags, error) {
	var (
		upfile    = flag.String("f", "Upfile", "path to upfile")
		inventory = flag.String("i", "inventory.json", "path to inventory")
		command   = flag.String("c", "", "command to run in upfile (use - to read from stdin)")
		tags      = flag.String("t", "", "tags from inventory to run (defaults to the name of the command)")
		serial    = flag.Int("n", 1, "how many of each type of server to operate on at a time")
		directory = flag.String("d", ".", "directory for checksum")
		prompt    = flag.Bool("p", false, "prompt before moving to the next batch")
		verbose   = flag.Bool("v", false, "verbose logs full commands")
	)
	flag.Parse()

	if *command == "" && *upfile != "-" {
		return flags{}, errors.New("missing command")
	}

	lim := map[string]struct{}{}
	if *tags != "" {
		lims := strings.Split(*tags, ",")
		if len(lims) > 0 {
			all := false
			for _, service := range lims {
				if service == "all" {
					lim["all"] = struct{}{}
					all = true
				}
			}
			if all && len(lims) > 1 {
				return flags{}, errors.New("cannot use 'all' tag alongside others")
			}
			for _, service := range lims {
				lim[service] = struct{}{}
			}
		}
	}
	extraVars := map[string]string{}
	for _, pair := range os.Environ() {
		if len(pair) == 0 {
			continue
		}
		pair = strings.TrimSpace(pair)
		vals := strings.Split(pair, "=")
		if len(vals) != 2 {
			continue
		}
		extraVars[vals[0]] = vals[1]
	}
	flgs := flags{
		Tags:      lim,
		Upfile:    *upfile,
		Inventory: *inventory,
		Serial:    *serial,
		Directory: *directory,
		Command:   up.CmdName(*command),
		Vars:      extraVars,
		Stdin:     *upfile == "-",
		Verbose:   *verbose,
		Prompt:    *prompt,
	}
	return flgs, nil
}

func makeBatches(
	conf *up.Config,
	inventory up.Inventory,
	max int,
) (batch, error) {
	batches := batch{}

	// Organize by tags, rather than IPs for efficiency in this next
	// operation
	invMap := map[string][]string{}
	for ip, tags := range inventory {
		for _, tag := range tags {
			if _, exist := invMap[tag]; !exist {
				invMap[tag] = []string{}
			}
			invMap[tag] = append(invMap[tag], ip)
		}
	}

	// Now create batches for each tag
	for tag, ips := range invMap {
		if max == 0 {
			batches[tag] = [][]string{ips}
			continue
		}
		b := [][]string{}
		for _, ip := range ips {
			b = appendToBatch(b, ip, max)
		}
		batches[tag] = b
	}
	if len(batches) == 0 {
		return nil, errors.New("empty batches, nothing to do")
	}
	return batches, nil
}

// appendToBatch adds to the existing last batch if smaller than the max size.
// Otherwise it creates and appends a new batch to the end.
func appendToBatch(b [][]string, srv string, max int) [][]string {
	if len(b) == 0 {
		return [][]string{{srv}}
	}
	last := b[len(b)-1]
	if len(last) >= max {
		return append(b, []string{srv})
	}
	b[len(b)-1] = append(last, srv)
	return b
}

func calcChecksum(dir string) (string, error) {
	files := []string{}
	err := filepath.Walk(dir, func(pth string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		name := info.Name()
		if strings.HasPrefix(name, ".") && name != "." {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, pth)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk filepath: %w", err)
	}
	h := sha256.New()
	for _, pth := range files {
		fi, err := os.Open(pth)
		if err != nil {
			return "", fmt.Errorf("checksum: open file: %w", err)
		}
		if _, err = io.Copy(h, fi); err != nil {
			fi.Close()
			return "", fmt.Errorf("checksum: copy: %w", err)
		}
		if err = fi.Close(); err != nil {
			return "", fmt.Errorf("checksum: close: %w", err)
		}
	}
	sum := h.Sum(nil)
	if len(sum) == 0 {
		return "", errors.New("empty checksum")
	}
	return base64.URLEncoding.EncodeToString(sum), nil
}

func randomizeOrder(ss []string) []string {
	out := make([]string, len(ss))
	perm := rand.Perm(len(ss))
	for i, p := range perm {
		out[i] = ss[p]
	}
	return out
}

// substituteVariables recursively up to 10 times. After 10 substitutions, this
// function reports an error.
func substituteVariables(
	vars map[string]string,
	cmds map[up.CmdName]*up.Cmd,
	cmd string,
) (string, error) {
	replacements := []string{}
	for cmdName, cmd := range cmds {
		if len(cmd.ExecIfs) > 0 {
			continue
		}
		replacements = append(replacements, "$"+string(cmdName))
		rep := ""
		for _, c := range cmd.Execs {
			rep += c + "\n"
		}
		rep = strings.TrimSpace(rep)
		replacements = append(replacements, rep)
	}
	for name, val := range vars {
		replacements = append(replacements, "$"+name)
		replacements = append(replacements, val)
	}
	r := strings.NewReplacer(replacements...)
	for i := 0; i < 10; i++ {
		tmp := r.Replace(cmd)
		if cmd == tmp {
			// We're done
			return cmd, nil
		}
		cmd = tmp
	}
	return "", errors.New("possible cycle detected")
}

func copyCommands(m1 map[up.CmdName]*up.Cmd) map[up.CmdName]*up.Cmd {
	m2 := map[up.CmdName]*up.Cmd{}
	for k, v := range m1 {
		m2[k] = v
	}
	return m2
}

// usage prints usage instructions. It passes through any error to be sent to
// Stderr by main().
func usage(err error) error {
	fmt.Println(`USAGE
	up -c <cmd> [options...]
	up -f -     [options...]

OPTIONS
	[-c] command to run in upfile
	[-f] path to Upfile, default "Upfile" or use "-" to read from stdin
	[-h] short-form help with flags
	[-i] path to inventory, default "inventory.json"
	[-n] number of servers to execute in parallel, default 1
	[-p] prompt before moving to next batch, default false
	[-t] comma-separated tags from inventory to execute, default is your command
	[-v] verbose output, default false

UPFILE
	Upfiles define the steps to be run for each server using a syntax
	similar to Makefiles.

	There are 4 parts to Upfiles:

	1. Command name: This is passed into up using "-c"
	2. Conditionals: Before running commands, up will execute
	   space-separated conditionals in order. It will proceed to run
	   commands for the server if and only if any of the conditionals
	   return a non-zero exit code. Conditionals are optional.
	3. Commands: One or more commands to be run if all conditionals pass.
	4. Variables: Variables can be substituted within commands by prefixing
	   the name with "$". Variable substitution values may be a single
	   value or an entire series of commands.

	These parts are generally arranged as follows:

	CMD_NAME_1 CONDITIONAL_1 CONDITIONAL_2
		CMD_1
		CMD_2

	CMD_NAME_2
		CMD_3
		$VARIABLE_1

	CONDITIONAL_1
		CMD_4

	VARIABLE_1
		SUBSTITUTION_VALUE

INVENTORY
	The inventory is a JSON file which maps IP addresses to arbitrary tags.
	It has the following format:

	{
		"IP_1": ["TAG_1", "TAG_2"],
		"IP_2": ["TAG_1"]
	}

	Because this is a simple JSON file, your inventory can be dynamically
	generated if you wish based on the state of your architecture at a
	given moment, or you can commit the single into source code alongside
	your Upfile.

EXIT STATUS
	up exits with 0 on success or 1 on any failure.

EXAMPLES
	In the following example Upfile, "deploy_dashboard" is the command.
	Before running the script indented underneath the command, up will
	first execute any space-separated commands to the right. In this
	example, up will only execute if check_version returns a non-zero exit
	code.

	All steps are executed locally, so to run commands on the server we use
	the reserved variable "$server" to get the IP and execute commands
	through ssh.

	$ cat Upfile
	deploy_dashboard check_version
		rsync -a dashboard $remote:
		ssh $remote 'sudo service dashboard restart'
		sleep 1 && $check_health

	check_health
		curl -s --max-time 1 $server/health

	check_version
		expr $CHECKSUM == "$(curl --max-time 1 $server/version)"

	remote
		$UP_USER@$server

	$ cat inventory.json
	{
		"10.0.0.1": ["dashboard", "redis", "openbsd"]
		"10.0.0.2": ["dashboard", "openbsd"]
		"10.0.0.3": ["dashboard_staging"],
		"10.0.0.4": ["postgres", "debian"],
		"10.0.0.5": ["reverse_proxy"]
	}

	Good inventory tags are generally the type of services running on the
	box and the operating system (for easily updating groups of machines
	with the same OS).

	In this example, running:

	$ up -c deploy_dashboard -t dashboard

	would execute deploy_dashboard on 10.0.0.1 and 10.0.0.2. Since we
	didn't pass in "-n 2", up will deploy on the first server before
	continuing to the next.

AUTHORS
	up was written by Evan Tann <up@evantann.com>.`)

	return err
}
