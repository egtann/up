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
		log.Printf("running %s on %s\n", conf.DefaultCommand,
			conf.DefaultEnvironment)
	} else {
		lims := []string{}
		for lim := range flgs.Limit {
			lims = append(lims, string(lim))
		}
		tmp := strings.Join(lims, ", ")
		log.Printf("running %s on %s\n", conf.DefaultCommand, tmp)
	}

	// Remove any unnecessary inventory. All remaining defined inventory
	// will be used.
	for invName := range conf.Inventory {
		if _, exist := flgs.Limit[invName]; !exist {
			delete(conf.Inventory, invName)
		}
	}

	// Validate all limits are defined in inventory (i.e. no silent failure
	// on typos).
	if len(flgs.Limit) > len(conf.Inventory) {
		// TODO improve error message to specify which limits are
		// undefined
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
	succeeds, fails := []string{}, []result{}
	for _, srvBatch := range batches {
		go func(srvBatch [][]string) {
			for _, srvGroup := range srvBatch {
				ch := make(chan result, len(srvGroup))
				srvGroup = randomizeOrder(srvGroup)
				cmd := conf.Commands[conf.DefaultCommand]
				runExecIfs(ch, conf.Commands, cmd, chk, srvGroup)
				for i := 0; i < len(srvGroup); i++ {
					res := <-ch
					if res.err == nil {
						succeeds = append(succeeds, res.server)
					} else {
						fails = append(fails, res)
					}
				}
				close(ch)
			}
			done <- true
		}(srvBatch)
	}
	for i := 0; i < len(batches); i++ {
		<-done
	}
	if len(fails) == 0 {
		log.Println("success")
		os.Exit(0)
	}
	log.Printf("\nfailed to start some services\n\n")
	log.Println("succeeded:")
	for _, s := range succeeds {
		log.Println(s)
	}
	log.Printf("\n\n")
	log.Println("failed:")
	for _, f := range fails {
		log.Printf("%s: %s\n", f.server, f.err)
	}
	os.Exit(1)
}

func runExecIfs(
	ch chan result,
	cmds map[up.CmdName]*up.Cmd,
	cmd *up.Cmd,
	chk string,
	servers []string,
) {
	send := func(ch chan result, err error, servers []string) {
		for _, srv := range servers {
			ch <- result{server: srv, err: err}
		}
	}
	var needToRun bool
	for _, execIf := range cmd.ExecIfs {
		// TODO should this also enforce ExecIfs? Probably...
		// TODO this should handle errors correctly through the channel
		steps := cmds[execIf].Execs
		for _, step := range steps {
			ok, err := runExec(cmds, step, chk, servers, true)
			if err != nil {
				send(ch, err, servers)
				return
			}
			if !ok {
				needToRun = true
			}
		}
	}
	if !needToRun {
		for _, srv := range servers {
			ch <- result{server: srv}
		}
		return
	}
	for _, cmdLine := range cmd.Execs {
		_, err := runExec(cmds, cmdLine, chk, servers, false)
		if err != nil {
			send(ch, err, servers)
			return
		}
	}
	send(ch, nil, servers)
}

// runExec reports whether all execIfs passed and an error if any.
func runExec(
	cmds map[up.CmdName]*up.Cmd,
	cmd, chk string,
	servers []string,
	execIf bool,
) (bool, error) {
	cmds["checksum"] = &up.Cmd{Execs: []string{chk}}
	for _, server := range servers {
		// TODO ensure that no cycles are present with depth-first
		// search

		// Now substitute any variables designated by a '$'
		cmds["server"] = &up.Cmd{Execs: []string{server}}
		cmd, err := substituteVariables(cmds, cmd)
		if err != nil {
			return false, errors.Wrap(err, "substitute")
		}

		log.Printf("[%s] %s\n", server, cmd)
		c := exec.Command("sh", "-c", cmd)
		out, err := c.CombinedOutput()
		if err != nil {
			if execIf {
				// TODO log if verbose
				return false, nil
			}
			err = fmt.Errorf("%s: %q", err, string(out))
			return false, err
		}
		// TODO log if verbose
		// log.Println(string(out))
	}
	return true, nil
}

// parseFlags and validate them.
func parseFlags() (flags, error) {
	f := flag.NewFlagSet("flags", flag.ExitOnError)
	upfile := f.String("u", "Upfile", "path to upfile")
	directory := f.String("d", ".", "directory for checksum")
	limit := f.String("l", "", "limit to specific services")
	serial := f.Int("n", 1, "how many of each type of server to operate on at a time")
	vars := f.String("x", "", "comma-separated extra vars for commands, "+
		"e.g. Color=Red,Font=Small")
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
	if len(lims) > 0 && lims[0] != "" {
		for _, service := range lims {
			lim[up.InvName(service)] = struct{}{}
		}
	}
	varList := strings.Split(*vars, ",")
	extraVars := map[string]string{}
	for _, pair := range varList {
		if len(pair) == 0 {
			continue
		}
		vals := strings.Split(pair, "=")
		if len(vals) != 2 {
			return flags{}, errors.New("invalid extra var")
		}
		extraVars[vals[0]] = vals[1]
	}
	flgs := flags{
		Limit:     lim,
		Upfile:    *upfile,
		Serial:    *serial,
		Directory: *directory,
		Command:   up.CmdName(cmd),
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
