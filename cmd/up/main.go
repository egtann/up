// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"flag"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
)

type serviceConfig struct {
	// CmdDir from which Start commands will run.
	CmdDir string `toml:"cmd_dir"`

	// IPs which up will start.
	IPs []string

	// Start is a series of commands which up will run on each provided IP.
	// Go templating is available within them.
	Start []string

	// User for the remote server. This is made available in the commands
	// for templating and used to create an additional helper {{.Remote}}.
	// {{.Remote}} is defined as {{.User}}@{{.IP}}
	User string

	// HealthCheckURL which should reply with HTTP status code 200 when
	// available.
	HealthCheckURL string `toml:"health_check_url"`

	// HealthCheckDelay waits a specific number of seconds before checking
	// health of the started service. This gives the machine time a
	// reasonable window to boot. Default 0.
	HealthCheckDelay int `toml:"health_check_delay"`

	// Serial is the max number of servers contained in a batch. Default 0,
	// which assigns all IPs to a single batch.
	Serial uint

	// VersionCheckURL on the remote server which replies with a sha256
	// hash that's calculated at deploy time using VersionCheckDir.
	VersionCheckURL string `toml:"version_check_url"`

	// VersionCheckDir containing the service's source code. This directory
	// will be checksummed if VersionCheckURL is defined. If
	// VersionCheckURL is not defined, VersionCheckDir does nothing. If
	// VersionCheckURL is defined, but VersionCheckDir is not,
	// VersionCheckDir runs on the current directory. Hidden files and
	// folders (those beginning with '.') are excluded from any checksum.
	VersionCheckDir string `toml:"version_check_dir"`

	// VersionCheckCmd is an alternative way to check the version on a
	// remote server. Sometimes you're not able to add a /version endpoint.
	// In that case, remove version_check_url from your Upfile and replace
	// it with version_check_cmd. up expects exit code 0 if versions match
	// and exit >0 if they do not, which can be as simple as:
	//	grep -Fxq {{.Checksum}} ./checksum.file
	VersionCheckCmd string `toml:"version_check_cmd"`
}

type selfConfig struct {
	Config   *configuration
	IP       string
	Checksum string
	User     string
	Remote   string
	Vars     map[string]string
}

type flags struct {
	// Env is the environment which you wish to bring up, e.g. "staging" or
	// "production"
	Env string

	// Upfile allows you to specify a different Upfile name. This is
	// helpful when running across multiple operating systems or shells.
	// For example, you may have Upfile.windows.toml and Upfile.linux.toml,
	// or Upfile.bash.toml and Upfile.fish.toml.
	Upfile string

	// Dry run lists all commands that would be run without running them
	Dry bool

	// Verbose log output displays the output from commands run on each
	// server
	Verbose bool

	// Force start of server even when versions match
	Force bool

	// Limit the changed services to those enumerated if the flag is
	// provided
	Limit map[serviceType]struct{}

	// Vars passed into `up` at runtime to be used in start commands.
	Vars map[string]string
}

type serviceType = string

type configuration struct {
	Services map[serviceType]*serviceConfig
	Flags    flags
}

// batch maps a service to several groups of IPs
type batch map[serviceType][][]string

type result struct {
	err error
	ip  string
}

// TODO: show example Upfiles working across windows and linux dev environments
// in readme
func main() {
	// TODO flags: -x extra-vars file for passing in extra template data
	// without it remaining in source, like a secret API key for a health
	// check, or specific IPs for a blue-green deploy

	errLog := log.New(os.Stdout, "", log.Lshortfile)
	log.SetFlags(0)
	rand.Seed(time.Now().UnixNano())

	flgs, err := parseFlags()
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "parse flags"))
	}

	upfileData := map[string]map[serviceType]*serviceConfig{}
	if _, err = toml.DecodeFile(flgs.Upfile, &upfileData); err != nil {
		errLog.Fatal(errors.Wrap(err, "decode toml"))
	}

	services, exists := upfileData[flgs.Env]
	if !exists {
		err = fmt.Errorf("environment %s not in %s", flgs.Env, flgs.Upfile)
		errLog.Fatal(err)
	}
	if err = validateLimits(flgs.Limit, services, flgs.Env); err != nil {
		errLog.Fatal(errors.Wrap(err, "validate limits"))
	}
	conf := &configuration{Services: services, Flags: flgs}

	// Multiple batches for rolling deploy
	batches, err := makeBatches(conf.Services)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "make batches"))
	}
	if conf.Flags.Verbose {
		log.Printf("got batches: %s\n", batches)
	}

	// checksums maps each filepath to a sha256 checksum
	log.Println("calculating checksums")
	checksums, err := calcChecksums(conf.Services)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "calc checksum"))
	}
	log.Println(checksums)

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
	log.Printf("\nfailed to start some services\n")
	log.Printf("\nsucceeded: %s\n", succeeds)
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
	for _, cmd := range srv.Start {
		if err := startOne(conf, user, ip, typ, chk, cmd); err != nil {
			return errors.Wrap(err, "start ip")
		}
	}
	if srv.HealthCheckDelay > 0 {
		log.Printf("[%s] %s: waiting %d seconds for health check delay\n\n",
			conf.Flags.Env, typ, srv.HealthCheckDelay)
		delay := time.Duration(srv.HealthCheckDelay)
		time.Sleep(delay * time.Second)
	}
	return nil
}

func startOne(
	conf *configuration,
	user, ip string,
	typ serviceType,
	chk, cmd string,
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
	log.Printf("[%s] %s: start %s\n%s\n\n", conf.Flags.Env, typ, ip, cmd)
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
		log.Printf("health_check_url missing for %s. assuming failed\n", ip)
		return false, nil
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
	client := http.Client{Timeout: 10 * time.Second}
	cmd := string(buf.Bytes())
	var code int
	const attempts = 3
	for i := 0; i < attempts; i++ {
		log.Printf("[%s] %s: check_health %s (%d)\n%s\n\n",
			conf.Flags.Env, typ, ip, i+1, cmd)
		if conf.Flags.Dry {
			continue
		}
		req, err := http.NewRequest("GET", cmd, nil)
		if err != nil {
			return false, errors.Wrap(err, "new request")
		}
		resp, err := client.Do(req)
		if err != nil {
			return false, errors.Wrap(err, "request")
		}
		code = resp.StatusCode
		if code == http.StatusOK {
			break
		}
		if i < attempts-1 {
			time.Sleep(3 * time.Second)
		}
	}
	return code == http.StatusOK, nil
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
	log.Println("getting version at", url)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return false, errors.Wrap(err, "get version")
	}
	client := &http.Client{Timeout: 60 * time.Second}
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
		log.Printf("same version found for %s, skipping\n", ip)
		return true, nil
	}
	log.Printf("server version %q\n", string(body))
	log.Printf("local version %q\n", checksum)
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

func parseFlags() (flags, error) {
	if len(os.Args) <= 1 {
		return flags{}, errors.New("missing environment")
	}
	env := os.Args[1]
	f := flag.NewFlagSet("flags", flag.ExitOnError)
	upfile := f.String("u", "Upfile.toml", "path to upfile")
	dry := f.Bool("d", false, "dry run")
	verbose := f.Bool("v", false, "verbose output")
	force := f.Bool("f", false, "force start")
	limit := f.String("l", "",
		"limit to specific services")
	vars := f.String("x", "", "comma-separated extra vars for commands, "+
		"e.g. Color=Red,Font=Small")
	if len(os.Args) > 2 {
		f.Parse(os.Args[2:])
	} else {
		f.Parse(os.Args)
	}
	lim := map[serviceType]struct{}{}
	lims := strings.Split(*limit, ",")
	if len(lims) > 0 && lims[0] != "" {
		for _, service := range lims {
			lim[serviceType(service)] = struct{}{}
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
		Env:     env,
		Upfile:  *upfile,
		Dry:     *dry,
		Verbose: *verbose,
		Force:   *force,
		Limit:   lim,
		Vars:    extraVars,
	}
	return flgs, nil
}

func validateLimits(
	limits map[serviceType]struct{},
	services map[serviceType]*serviceConfig,
	env string,
) error {
	for serviceName, _ := range limits {
		if _, exists := services[serviceName]; !exists {
			return fmt.Errorf(
				"no service named %s in %s", serviceName, env)
		}
	}
	return nil
}

func calcDirChecksum(dir string) ([]byte, error) {
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
		if info.IsDir() {
			return nil
		}
		files = append(files, pth)
		return err
	})
	if err != nil {
		return nil, errors.Wrap(err, "walk filepath")
	}
	h := sha256.New()
	for _, pth := range files {
		fi, err := os.Open(pth)
		if err != nil {
			return nil, errors.Wrap(err, "open file")
		}
		if _, err = io.Copy(h, fi); err != nil {
			fi.Close()
			return nil, errors.Wrap(err, "copy file")
		}
		if err = fi.Close(); err != nil {
			return nil, errors.Wrap(err, "close file")
		}
	}
	sum := h.Sum(nil)
	if len(sum) == 0 {
		return nil, errors.New("empty checksum")
	}
	return sum, nil
}

func randomizeOrder(ss []string) []string {
	out := make([]string, len(ss))
	perm := rand.Perm(len(ss))
	for i, p := range perm {
		out[i] = ss[p]
	}
	return out
}

func makeBatches(conf map[serviceType]*serviceConfig) (batch, error) {
	batches := batch{}
	for typ, service := range conf {
		if len(service.IPs) == 0 {
			return nil, fmt.Errorf("no ips for %s", typ)
		}
		service.IPs = randomizeOrder(service.IPs)
		if service.Serial == 0 {
			batches[typ] = [][]string{service.IPs}
			continue
		}
		batchIdx := 0
		max := int(service.Serial)
		for _, ip := range service.IPs {
			if len(batches[typ]) <= batchIdx {
				bat := batches[typ]
				batches[typ] = append(bat, []string{ip})
			} else {
				bat := batches[typ][batchIdx]
				batches[typ][batchIdx] = append(bat, ip)
			}
			if len(batches[typ][batchIdx]) >= max {
				batchIdx++
			}
		}
	}
	if len(batches) == 0 {
		return nil, errors.New("empty batches, nothing to do")
	}
	return batches, nil
}

func calcChecksums(
	conf map[serviceType]*serviceConfig,
) (map[string]string, error) {
	chks := map[string]string{}
	for typ, service := range conf {
		if service.VersionCheckURL == "" && service.VersionCheckCmd == "" {
			if service.VersionCheckDir != "" {
				log.Printf("warn: VersionCheckDir defined on %s, but missing VersionCheckURL and VersionCheckCmd", typ)
				continue
			}
		}
		dir := service.VersionCheckDir
		if dir == "" {
			dir = "."
		}
		if _, exist := chks[dir]; !exist {
			checksum, err := calcDirChecksum(dir)
			if err != nil {
				return nil, errors.Wrap(err, "calc dir checksum")
			}
			chks[dir] = base64.URLEncoding.EncodeToString(checksum)
		}
	}
	return chks, nil
}

func addSelf(c *configuration, user, ip, checksum string) *selfConfig {
	sc := &selfConfig{
		Config:   c,
		IP:       ip,
		User:     user,
		Checksum: checksum,
		Remote:   user + "@" + ip,
	}
	return sc
}
