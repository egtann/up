// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"bytes"
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
	"text/template"
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

type tmplData struct {
	Server   string
	Checksum string
}

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
	if len(flgs.Limit) > 0 {
		for invName := range conf.Inventory {
			if _, exist := flgs.Limit[invName]; !exist {
				delete(conf.Inventory, invName)
			}
		}
	} else {
		lims := map[up.InvName]struct{}{}
		for invName := range conf.Inventory {
			lims[invName] = struct{}{}
		}
		flgs.Limit = lims
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

				// TODO not sending enough results back... debug why
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
	// TODO remove golang templates
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	for _, server := range servers {
		// First substitute any Golang template variables passed in via
		// -x or the ones already provided, Checksum and Server
		var byt []byte
		buf := bytes.NewBuffer(byt)
		err = tmpl.Execute(buf, self(server, string(chk)))
		if err != nil {
			return false, errors.Wrap(err, "execute template")
		}
		cmd = string(buf.Bytes())

		// TODO ensure that no cycles are present with depth-first
		// search

		// Now substitute any variables designated by a '$'
		cmd = substituteVariables(cmds, cmd)

		log.Println("running:", cmd)
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
		log.Println(string(out))
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

func self(server, checksum string) tmplData {
	return tmplData{
		Server:   server,
		Checksum: checksum,
	}
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

// substituteVariables recursively.
func substituteVariables(cmds map[up.CmdName]*up.Cmd, cmd string) string {
	// find first "$" that doesn't have a leading "\"

	fields := strings.Fields(cmd)
	for i, f := range fields {
		if !strings.HasPrefix(f, "$") {
			continue
		}

		// If the command doesn't exist, skip it (presumably
		// it's defined in their environment or some error will
		// occur when you try to run it)
		f = strings.TrimPrefix(f, "$")
		replace, exist := cmds[up.CmdName(f)]
		if !exist {
			log.Printf("warn: $%s is undefined in Upfile\n", f)
			continue
		}

		// At this point the command does exist. Ensure that anything
		// being replaced also has variable substitutions.
		for i, ex := range replace.Execs {
			replace.Execs[i] = substituteVariables(cmds, ex)
		}

		// Insert the desired command into the array and break out of
		// the loop. Insert is from
		// https://github.com/golang/go/wiki/SliceTricks#insert
		fields = append(fields, replace.Execs...)
		copy(fields[i+1:], fields[i:])
		for j, ex := range replace.Execs {
			fields[i+j] = ex
		}
		break
	}
	return strings.Join(fields, " ")
}

/*
	// Bring up each service type in parallel
	done := make(chan bool, len(batches))
	succeeds, fails := []string{}, []result{}
	for typ, ipgroups := range batches {
		go func(typ serviceType, ipgroups [][]string) {
			for _, ips := range ipgroups {
				log.Printf("[%s] %s: start batch: %s\n\n", conf.Flags.Env, typ, ips)

				// Start each batch of IPs concurrently
				ch := make(chan result, len(ips))
				startBatch(ch, conf, typ, ips, checksums)
				for i := 0; i < len(ips); i++ {
					res := <-ch
					if res.err == nil {
						succeeds = append(succeeds, res.ip)
					} else {
						fails = append(fails, res)
					}
				}
				close(ch)
			}
			done <- true
		}(typ, ipgroups)
	}
	for i := 0; i < len(batches); i++ {
		<-done
	}
	if len(fails) == 0 {
		log.Println("started all services")
		os.Exit(0)
	}
	if conf.Flags.Dry {
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
		log.Printf("%s: %s\n", f.ip, f.err)
	}
	os.Exit(1)
}

func start(
	conf *configuration,
	user, ip string,
	typ serviceType,
	chk string,
) error {
	srv := conf.Services[typ]
	for i, cmd := range srv.Start {
		err := startOne(conf, user, ip, typ, chk, cmd, i, len(srv.Start))
		if err != nil {
			return errors.Wrap(err, "start ip")
		}
	}
	if srv.HealthCheckDelay > 0 {
		log.Printf("[%s] %s: check health %s: waiting %d seconds for health check delay\n\n",
			conf.Flags.Env, typ, ip, srv.HealthCheckDelay)
		if !conf.Flags.Dry {
			delay := time.Duration(srv.HealthCheckDelay)
			time.Sleep(delay * time.Second)
		}
	}
	return nil
}

func startOne(
	conf *configuration,
	user, ip string,
	typ serviceType,
	chk, cmd string,
	cmdIdx, cmdsLen int,
) error {
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	err = tmpl.Execute(buf, addSelf(conf, user, ip, string(chk)))
	if err != nil {
		return errors.Wrap(err, "execute template")
	}
	cmd = string(buf.Bytes())
	log.Printf("[%s] %s: start %s (%d/%d)\n%s\n\n", conf.Flags.Env, typ,
		ip, cmdIdx+1, cmdsLen, strings.TrimSpace(cmd))
	if conf.Flags.Dry {
		return nil
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = conf.Services[typ].CmdDir
	out, err := c.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%s: %q", err, string(out))
		return errors.Wrap(err, "run cmd")
	}
	if conf.Flags.Verbose {
		log.Println(string(out))
	}
	return nil
}

func checkHealth(
	conf *configuration,
	user, ip string,
	typ serviceType,
) (bool, error) {
	tmplCmd := conf.Services[typ].HealthCheckURL
	if len(tmplCmd) == 0 {
		log.Printf("health_check_url missing for %s. assuming succeeded\n", ip)
		return true, nil
	}
	tmpl, err := template.New("").Parse(tmplCmd)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, user, ip, "")); err != nil {
		return false, errors.Wrap(err, "execute template")
	}
	client := &http.Client{Timeout: httpTimeout}
	cmd := string(buf.Bytes())
	if conf.Flags.Dry {
		log.Printf("[%s] %s: check health %s\n\n", conf.Flags.Env, typ,
			cmd)
		return false, nil
	}
	req, err := http.NewRequest("GET", cmd, nil)
	if err != nil {
		return false, errors.Wrap(err, "new request")
	}
	resp, err := client.Do(req)
	if err != nil {
		return false, errors.Wrap(err, "request")
	}
	log.Printf("[%s] %s: check health %s\n%s\n\n", conf.Flags.Env, typ,
		cmd, resp.Status)
	return resp.StatusCode == http.StatusOK, nil
}

func checkVersionURL(
	conf *configuration,
	user, ip string,
	typ serviceType,
	urlTmpl string,
	checksum string,
) (bool, error) {
	if len(urlTmpl) == 0 || len(checksum) == 0 {
		return false, nil
	}
	tmpl, err := template.New("").Parse(urlTmpl)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, user, ip, checksum)); err != nil {
		return false, errors.Wrap(err, "execute template")
	}
	url := string(buf.Bytes())
	log.Printf("[%s] %s: get version %s\n", conf.Flags.Env, typ, ip)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, errors.Wrap(err, "get version")
	}
	client := &http.Client{Timeout: httpTimeout}
	rsp, err := client.Do(req)
	if err != nil {
		return false, errors.Wrap(err, "make request")
	}
	if rsp.StatusCode != http.StatusOK {
		log.Printf(
			"unexpected check_version response code %d, wanted 200\n",
			rsp.StatusCode)
		log.Println("starting server")
		return false, nil
	}
	body, err := ioutil.ReadAll(rsp.Body)
	if err != nil {
		return false, errors.Wrap(err, "read resp body")
	}
	if string(body) == checksum {
		log.Printf("same version, skipping\n\n")
		return true, nil
	}
	log.Printf("new version, updating\n\n")
	return false, nil
}

func checkVersionCmd(
	conf *configuration,
	user, ip string,
	typ serviceType,
	cmdTmpl, checksum string,
) (bool, error) {
	if len(cmdTmpl) == 0 || len(checksum) == 0 {
		return false, nil
	}
	tmpl, err := template.New("").Parse(cmdTmpl)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, user, ip, checksum)); err != nil {
		return false, errors.Wrap(err, "execute template")
	}
	cmd := string(buf.Bytes())
	log.Printf("[%s] %s: check_version_cmd %s\n%s\n\n", conf.Flags.Env, typ, ip, cmd)
	if conf.Flags.Dry {
		return false, nil
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = conf.Services[typ].CmdDir
	out, err := c.CombinedOutput()
	if conf.Flags.Verbose {
		log.Println(string(out))
	}
	if err != nil {
		// Log but don't error out, just assume that the version
		// doesn't match given an exit code > 0
		log.Println(err)
		return false, nil
	}
	return true, nil
}

func startBatch(
	ch chan result,
	conf *configuration,
	typ serviceType,
	ips []string,
	checksums map[string]string,
) {
	srv := conf.Services[typ]
	versionCheckDir := srv.VersionCheckDir
	if len(versionCheckDir) == 0 {
		versionCheckDir = "."
	}
	chk := checksums[versionCheckDir]
	user := srv.User
	for _, ip := range ips {
		if len(conf.Flags.Limit) > 0 {
			_, exists := conf.Flags.Limit[typ]
			if !exists {
				ch <- result{ip: ip}
				continue
			}
		}
		go func(ip string, typ serviceType) {
			// Then check version, and update if needed, then check
			// health again (on delay)
			ok, err := checkHealth(conf, user, ip, typ)
			if err != nil {
				// Log the error, but since we haven't started
				// yet we'll just consider it not ok and try to
				// boot
				log.Printf("[%s] %s: failed health check: %s\n\n", conf.Flags.Env, typ, ip)
				ok = false
			}
			if !ok {
				err = start(conf, user, ip, typ, chk)
				if err != nil {
					ch <- result{
						err: errors.Wrap(err, "update"),
						ip:  ip,
					}
					return
				}
				ok, err = checkHealth(conf, user, ip, typ)
				if !ok && err == nil {
					err = errors.New("failed start (bad health check)")
				}
				ch <- result{
					err: err,
					ip:  ip,
				}
				return
			}
			if !conf.Flags.Force {
				ul := srv.VersionCheckURL
				vcCmd := srv.VersionCheckCmd
				var vtype string
				if len(ul) > 0 && len(vcCmd) > 0 {
					ch <- result{
						err: errors.New("cannot define both version_check_url and version_check_cmd in upfile"),
						ip:  ip,
					}
					return
				} else if len(ul) > 0 {
					vtype = "url"
					ok, err = checkVersionURL(conf, user, ip, typ, ul, chk)
				} else if len(vcCmd) > 0 {
					vtype = "cmd"
					ok, err = checkVersionCmd(conf, user, ip, typ, vcCmd, chk)
				}
				if err != nil {
					ch <- result{
						err: errors.Wrapf(err,
							"failed check_version %s %s", vtype, ip),
						ip: ip,
					}
					return
				}
				if ok {
					ch <- result{ip: ip}
					return
				}
			}
			err = start(conf, user, ip, typ, chk)
			if err != nil {
				ch <- result{
					err: errors.Wrap(err, "update"),
					ip:  ip,
				}
				return
			}
			ok, err = checkHealth(conf, user, ip, typ)
			if !ok && err == nil {
				err = errors.New("failed start (bad health check)")
			}
			ch <- result{
				err: err,
				ip:  ip,
			}
			return
		}(ip, typ)
	}
}





*/
