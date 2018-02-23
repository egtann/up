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
	// CmdDir from which Start and HealthCheck commands will run.
	CmdDir           string `toml:"cmd_dir"`
	IPs              []string
	Start            []string
	HealthCheckPath  string `toml:"health_check_path"`
	HealthCheckDelay int    `toml:"health_check_delay"`
	Serial           uint
	VersionCheckPath string `toml:"version_check_path"`

	// VersionCheckDir containing the service's source code. This directory
	// will be checksummed if VersionCheckPath is defined. If
	// VersionCheckPath is not defined, VersionCheckDir does nothing. If
	// VersionCheckPath is defined, but VersionCheckDir is not,
	// VersionCheckDir runs on the current directory. Hidden files and
	// folders (those beginning with '.') are excluded from any checksum.
	VersionCheckDir string `toml:"version_check_dir"`
}

type selfConfig struct {
	Config *configuration
	Self   struct {
		IP       string
		Checksum string
	}
	Vars map[string]string
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
	checksums, err := calcChecksums(conf.Services)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "calc checksum"))
	}

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
	ip string,
	typ serviceType,
	chk string,
) error {
	srv := conf.Services[typ]
	for _, cmd := range srv.Start {
		if err := startOne(conf, ip, typ, chk, cmd); err != nil {
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
	ip string,
	typ serviceType,
	chk, cmd string,
) error {
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	err = tmpl.Execute(buf, addSelf(conf, ip, string(chk)))
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
	ip string,
	typ serviceType,
) (bool, error) {
	tmplCmd := conf.Services[typ].HealthCheckPath
	tmpl, err := template.New("").Parse(tmplCmd)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, ip, "")); err != nil {
		return false, errors.Wrap(err, "execute template")
	}
	client := http.Client{Timeout: 10 * time.Second}
	cmd := string(buf.Bytes())
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
		if resp.StatusCode == http.StatusOK {
			break
		}
		if i < attempts-1 {
			time.Sleep(3 * time.Second)
		}
	}
	return err == nil, nil
}

func checkVersion(
	conf *configuration,
	ip string,
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
	if err = tmpl.Execute(buf, addSelf(conf, ip, "")); err != nil {
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

func startBatch(
	ch chan result,
	conf *configuration,
	typ serviceType,
	ips []string,
	checksums map[string]string,
) {
	versionCheckDir := conf.Services[typ].VersionCheckDir
	if len(versionCheckDir) == 0 {
		versionCheckDir = "."
	}
	chk := checksums[versionCheckDir]
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
			ok, err := checkHealth(conf, ip, typ)
			if err != nil {
				ch <- result{
					err: errors.Wrapf(err,
						"failed check_health %s", ip),
					ip: ip,
				}
				return
			}
			if !ok {
				err = start(conf, ip, typ, chk)
				if err != nil {
					ch <- result{
						err: errors.Wrap(err, "update"),
						ip:  ip,
					}
					return
				}
				ok, err = checkHealth(conf, ip, typ)
				if !ok && err == nil {
					err = errors.New("failed start (bad health check)")
				}
				ch <- result{
					err: err,
					ip:  ip,
				}
				return
			}
			ul := conf.Services[typ].VersionCheckPath
			ok, err = checkVersion(conf, ip, typ, ul, chk)
			if err != nil {
				ch <- result{
					err: errors.Wrapf(err,
						"failed check_version %s", ip),
					ip: ip,
				}
				return
			}
			if ok {
				ch <- result{ip: ip}
				return
			}
			err = start(conf, ip, typ, chk)
			ch <- result{
				err: errors.Wrap(err, "update"),
				ip:  ip,
			}
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
		if strings.HasPrefix(info.Name(), ".") {
			if info.IsDir() {
				return filepath.SkipDir
			}
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
	return h.Sum(nil), nil
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
		if service.VersionCheckPath == "" {
			if service.VersionCheckDir != "" {
				log.Printf("warn: VersionCheckDir defined on %s, but missing VersionCheckPath", typ)
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

func addSelf(c *configuration, ip, checksum string) *selfConfig {
	sc := &selfConfig{
		Config: c,
		Self: struct {
			IP       string
			Checksum string
		}{
			IP:       ip,
			Checksum: checksum,
		},
	}
	return sc
}
