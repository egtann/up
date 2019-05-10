// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"crypto/sha256"
	"encoding/base64"
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
	"github.com/pkg/errors"
)

type flags struct {
	// Upfile allows you to specify a different Upfile name. This is
	// helpful when running across multiple operating systems or shells.
	// For example, you may have Upfile.windows.toml and Upfile.linux.toml,
	// or Upfile.bash.toml and Upfile.fish.toml.
	Upfile string

	// Command to run. Like `make`, an empty Command defaults to the first
	// command in the Upfile.
	Command up.CmdName

	// Limit the changed services to those enumerated if the flag is
	// provided
	Limit map[up.InvName]struct{}

	// Serial determines how many servers of the same type will be operated
	// on at any one time. This defaults to 1. Use 0 to specify all of
	// them.
	Serial int

	// Directory used to calculate the checksum. Defaults to the current
	// directory.
	Directory string

	// Vars passed into `up` at runtime to be used in start commands.
	Vars map[string]string
}

type batch map[up.InvName][][]string

type result struct {
	server string
	err    error
}

func main() {
	errLog := log.New(os.Stdout, "", log.Lshortfile)
	log.SetFlags(0)
	rand.Seed(time.Now().UnixNano())

	flgs, err := parseFlags()
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "parse flags"))
	}

	fi, err := os.Open(flgs.Upfile)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "open upfile"))
	}
	defer fi.Close()
	conf, err := up.Parse(fi)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "parse upfile"))
	}
	if flgs.Command != "" {
		conf.DefaultCommand = flgs.Command
		if _, exist := conf.Commands[conf.DefaultCommand]; !exist {
			errLog.Fatal(fmt.Errorf("command %s is not defined",
				conf.DefaultCommand))
		}
	}
	if len(flgs.Limit) == 0 {
		flgs.Limit[conf.DefaultEnvironment] = struct{}{}
		log.Printf("running %s on default %s\n", conf.DefaultCommand,
			conf.DefaultEnvironment)
	} else {
		lims := []string{}
		for lim := range flgs.Limit {
			lims = append(lims, string(lim))
		}
		tmp := strings.Join(lims, ", ")
		log.Printf("running %s on %s\n", conf.DefaultCommand, tmp)
	}

	if _, exist := conf.Inventory["all"]; exist {
		errLog.Fatal(errors.New("reserved keyword 'all' cannot be inventory name"))
	}

	// Remove any unnecessary inventory. All remaining defined inventory
	// will be used.
	if _, exist := flgs.Limit["all"]; !exist {
		for invName := range conf.Inventory {
			if _, exist := flgs.Limit[invName]; !exist {
				delete(conf.Inventory, invName)
			}
		}
	}

	// Validate all limits are defined in inventory (i.e. no silent failure
	// on typos).
	if len(flgs.Limit) > len(conf.Inventory) {
		// TODO improve error message to specify which limits are
		// undefined
		log.Printf("limit: %+v\n", flgs.Limit)
		log.Printf("inventory: %+v\n", conf.Inventory)
		errLog.Fatal(errors.New("undefined limits"))
	}

	// Calculate a sha256 checksum on the provided directory (defaults to
	// current directory).
	log.Printf("calculating checksum\n")
	chk, err := calcChecksum(flgs.Directory)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "calc checksum"))
	}

	// Split into batches limited in size by the provided Serial flag.
	batches, err := makeBatches(conf, flgs.Serial)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "make batches"))
	}
	log.Printf("got batches: %v\n", batches)

	// For each batch, run the ExecIfs and run Execs if necessary.
	done := make(chan bool, len(batches))
	for _, srvBatch := range batches {
		go func(srvBatch [][]string) {
			for _, srvGroup := range srvBatch {
				ch := make(chan result, len(srvGroup))
				srvGroup = randomizeOrder(srvGroup)
				cmd := conf.Commands[conf.DefaultCommand]
				runExecIfs(ch, flgs.Vars, conf.Commands, cmd, chk, srvGroup)
				for i := 0; i < len(srvGroup); i++ {
					res := <-ch
					if res.err != nil {
						// Crash as soon as anything
						// fails
						errLog.Fatal(res.err)
						os.Exit(1)
					}
				}
			}
			done <- true
		}(srvBatch)
	}
	for i := 0; i < len(batches); i++ {
		<-done
	}
	log.Println("success")
}

func runExecIfs(
	ch chan result,
	vars map[string]string,
	cmds map[up.CmdName]*up.Cmd,
	cmd *up.Cmd,
	chk string,
	servers []string,
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
			ok, err := runExec(vars, cmds, step, chk, servers, true)
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
			_, err = runExec(vars, cmds, cmdLine, chk, servers, false)
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
	execIf bool,
) (bool, error) {
	cmds = copyCommands(cmds)
	cmds["checksum"] = &up.Cmd{Execs: []string{chk}}
	ch := make(chan runResult, len(servers))
	for _, server := range servers {
		go run(ch, vars, cmds, cmd, chk, server, execIf)
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

func run(
	ch chan<- runResult,
	vars map[string]string,
	cmds map[up.CmdName]*up.Cmd,
	cmd, chk, server string,
	execIf bool,
) {
	// TODO ensure that no cycles are present with depth-first
	// search

	// Now substitute any variables designated by a '$'
	cmds = copyCommands(cmds)
	cmds["server"] = &up.Cmd{Execs: []string{server}}
	cmd, err := substituteVariables(vars, cmds, cmd)
	if err != nil {
		err = errors.Wrap(err, "substitute")
		ch <- runResult{pass: false, error: err}
		return
	}

	log.Printf("[%s] %s\n", server, cmd)
	c := exec.Command("sh", "-c", cmd)
	c.Stdout = os.Stdout
	c.Stdin = os.Stdin
	if err = c.Run(); err != nil {
		if execIf {
			// TODO log if verbose
			ch <- runResult{pass: false}
			return
		}
		ch <- runResult{pass: false, error: err}
		return
	}
	ch <- runResult{pass: true}
}

// parseFlags and validate them.
func parseFlags() (flags, error) {
	f := flag.NewFlagSet("flags", flag.ExitOnError)
	upfile := f.String("u", "Upfile", "path to upfile")
	directory := f.String("d", ".", "directory for checksum")
	limit := f.String("l", "", "limit to specific services")
	serial := f.Int("n", 1, "how many of each type of server to operate on at a time")
	cmd := ""
	args := os.Args
	if len(args) == 2 {
		cmd = os.Args[1]
		if strings.HasPrefix(cmd, "-") {
			cmd = ""
			args = args[1:]
		} else {
			args = args[2:]
		}
	} else if len(args) > 2 {
		cmd = os.Args[1]
		if strings.HasPrefix(cmd, "-") {
			cmd = ""
			args = args[1:]
		} else {
			args = args[2:]
		}
	}
	f.Parse(args)
	lim := map[up.InvName]struct{}{}
	lims := strings.Split(*limit, ",")
	if len(lims) > 0 {
		all := false
		for _, service := range lims {
			if service == "all" {
				lim["all"] = struct{}{}
				all = true
			}
		}
		if all && len(lims) > 1 {
			return flags{}, errors.New("cannot use 'all' limit alongside others")
		}
		for _, service := range lims {
			lim[up.InvName(service)] = struct{}{}
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
		Limit:     lim,
		Upfile:    *upfile,
		Serial:    *serial,
		Directory: *directory,
		Command:   up.CmdName(cmd),
		Vars:      extraVars,
	}
	return flgs, nil
}

func makeBatches(conf *up.Config, max int) (batch, error) {
	batches := batch{}
	for invName, servers := range conf.Inventory {
		if max == 0 {
			batches[invName] = [][]string{servers}
			continue
		}
		b := batches[invName]
		b = [][]string{}
		for _, srv := range servers {
			b = appendToBatch(b, srv, max)
		}
		batches[invName] = b
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
		return "", errors.Wrap(err, "walk filepath")
	}
	h := sha256.New()
	for _, pth := range files {
		fi, err := os.Open(pth)
		if err != nil {
			return "", errors.Wrap(err, "open file")
		}
		if _, err = io.Copy(h, fi); err != nil {
			fi.Close()
			return "", errors.Wrap(err, "copy file")
		}
		if err = fi.Close(); err != nil {
			return "", errors.Wrap(err, "close file")
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
