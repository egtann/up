// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"bytes"
	"crypto/sha256"
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
	// CmdDir from which Provision, Update, and HealthCheck commands will
	// run.
	CmdDir           string `toml:"cmd_dir"`
	IPs              []string
	Provision        []string
	Update           []string
	HealthCheck      []string `toml:"health_check"`
	HealthCheckDelay int      `toml:"health_check_delay"`
	Serial           uint
	VersionCheckURL  string

	// VersionCheckDir containing the service's source code. This directory
	// will be checksummed if VersionCheck is defined. If VersionCheck is
	// not defined, VersionCheckDir does nothing. If VersionCheck is
	// defined, but VersionCheckDir is not, VersionCheckDir runs on the
	// current directory. Hidden files and folders (i.e. those beginning
	// with '.') are excluded from any checksum.
	VersionCheckDir string
}

type selfConfig struct {
	Config *configuration
	Self   struct{ IP string }
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

	// Force re-provisioning and updating for all servers, skipping version
	// and health checks
	Force bool

	// Limit the changed services to those enumerated if the flag is
	// provided
	Limit map[serviceType]struct{}
}

type serviceType string

type configuration struct {
	Services map[serviceType]*serviceConfig
	Flags    flags
}

// batch maps a service to several groups of IPs
type batch map[serviceType][][]string

// TODO: show example Upfiles working across windows and linux dev environments
// in readme
func main() {
	// TODO flags: -e extra-vars file for passing in extra template data
	// without it remaining in source, like a secret API key for a health
	// check, or specific IPs for a blue-green deploy

	log.SetFlags(0)
	rand.Seed(time.Now().UnixNano())

	flgs, err := parseFlags()
	if err != nil {
		log.Fatal(errors.Wrap(err, "parse flags"))
	}

	upfileData := map[string]map[serviceType]*serviceConfig{}
	if _, err = toml.DecodeFile(flgs.Upfile, &upfileData); err != nil {
		log.Fatal(errors.Wrap(err, "decode toml"))
	}

	services, exists := upfileData[flgs.Env]
	if !exists {
		err = fmt.Errorf("environment %s not in %s", flgs.Env, flgs.Upfile)
		log.Fatal(err)
	}
	if err = validateLimits(flgs.Limit, services, flgs.Env); err != nil {
		log.Fatal(errors.Wrap(err, "validate limits"))
	}
	conf := &configuration{Services: services, Flags: flgs}

	// Multiple batches for rolling deploy
	batches, err := makeBatches(conf.Services)
	if err != nil {
		log.Fatal(errors.Wrap(err, "make batches"))
	}
	if conf.Flags.Verbose {
		log.Printf("got batches: %s\n", batches)
	}

	// checksums maps each filepath to a sha256 checksum
	checksums, err := calcChecksums(conf.Services)
	if err != nil {
		log.Fatal(errors.Wrap(err, "calc checksum"))
	}

	// Bring up each service type in parallel
	done := make(chan bool, len(batches))
	for typ, ipgroups := range batches {
		go func(typ serviceType, ipgroups [][]string) {
			for _, ips := range ipgroups {
				log.Printf("[%s] %s: provision batch: %s\n", conf.Flags.Env, typ, ips)

				// Provision each batch of IPs concurrently
				ch := make(chan error, len(ips))
				provisionBatch(ch, conf, typ, ips, checksums)
				for i := 0; i < len(ips); i++ {
					if err = <-ch; err != nil {
						log.Fatal(errors.Wrapf(err, "provision batch %d", i))
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
	log.Println("success")
}

// provision a machine as a specific class, e.g. "web" or "loadbalancer"
func provision(
	conf *configuration,
	ip string,
	typ serviceType,
) error {
	for _, cmd := range conf.Services[typ].Provision {
		if err := provisionOne(conf, ip, typ, cmd); err != nil {
			return errors.Wrap(err, "provision")
		}
	}
	return nil
}

func provisionOne(
	conf *configuration,
	ip string,
	typ serviceType,
	cmd string,
) error {
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, ip)); err != nil {
		return errors.Wrap(err, "execute template")
	}
	cmd = string(buf.Bytes())
	log.Printf("[%s] %s: provision %s: %s\n", conf.Flags.Env, typ, ip, cmd)
	if !conf.Flags.Dry {
		c := exec.Command("sh", "-c", cmd)
		srv := conf.Services[typ]
		c.Dir = srv.CmdDir
		out, err := c.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%s: %s", err, string(out))
			return errors.Wrap(err, "run cmd")
		}
		if conf.Flags.Verbose {
			log.Println(string(out))
		}
		delay := time.Duration(srv.HealthCheckDelay)
		time.Sleep(delay * time.Second)
	}
	return nil
}

func update(
	conf *configuration,
	ip string,
	typ serviceType,
) error {
	for _, cmd := range conf.Services[typ].Update {
		if err := updateOne(conf, ip, typ, cmd); err != nil {
			return errors.Wrap(err, "update")
		}
	}
	return nil
}

func updateOne(
	conf *configuration,
	ip string,
	typ serviceType,
	cmd string,
) error {
	tmpl, err := template.New("").Parse(cmd)
	if err != nil {
		return errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, ip)); err != nil {
		return errors.Wrap(err, "execute template")
	}
	cmd = string(buf.Bytes())
	log.Printf("[%s] %s: update %s, %s\n", conf.Flags.Env, typ, ip, cmd)
	if conf.Flags.Dry {
		return nil
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = conf.Services[typ].CmdDir
	out, err := c.CombinedOutput()
	if err != nil {
		err = fmt.Errorf("%s: %s", err, string(out))
		return errors.Wrap(err, "run cmd")
	}
	if conf.Flags.Verbose {
		log.Println(string(out))
	}
	return nil
}

func checkHealthOne(
	conf *configuration,
	ip string,
	typ serviceType,
	tmplCmd string,
) (bool, error) {
	tmpl, err := template.New("").Parse(tmplCmd)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	var byt []byte
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, addSelf(conf, ip)); err != nil {
		return false, errors.Wrap(err, "execute template")
	}
	cmd := string(buf.Bytes())
	const attempts = 3
	var out []byte
	for i := 0; i < attempts; i++ {
		log.Printf("[%s] %s: check_health %s (%d): %s\n",
			conf.Flags.Env, typ, ip, i+1, cmd)
		if conf.Flags.Dry {
			continue
		}
		c := exec.Command("sh", "-c", cmd)
		c.Dir = conf.Services[typ].CmdDir
		out, err = c.CombinedOutput()
		if err == nil {
			break
		}
		if conf.Flags.Verbose {
			log.Printf("%s: %s\n", err, string(out))
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		err = fmt.Errorf("%s: %s", err, string(out))
		return false, errors.Wrap(err, "run cmd")
	}
	return true, nil
}

func checkHealth(
	conf *configuration,
	ip string,
	typ serviceType,
) (bool, error) {
	for _, cmd := range conf.Services[typ].HealthCheck {
		ok, err := checkHealthOne(conf, ip, typ, cmd)
		if err != nil || !ok {
			return false, errors.Wrap(err, "check health")
		}
	}
	return true, nil
}

func checkVersion(
	conf *configuration,
	ip string,
	typ serviceType,
	url string,
	checksum []byte,
) (bool, error) {
	if len(url) == 0 || len(checksum) == 0 {
		return false, nil
	}
	if !strings.HasPrefix(url, "/") {
		url = "/" + url
	}
	req, err := http.NewRequest("GET", ip+url, nil)
	if err != nil {
		return false, errors.Wrap(err, "get version")
	}
	client := &http.Client{Timeout: 60 * time.Second}
	rsp, err := client.Do(req)
	if err != nil {
		return false, errors.Wrap(err, "make request")
	}
	if rsp.StatusCode != http.StatusOK {
		return false, fmt.Errorf(
			"unexpected check_version response code %d, wanted 200",
			rsp.StatusCode)
	}
	body, err := ioutil.ReadAll(rsp.Body)
	if err != nil {
		return false, errors.Wrap(err, "read resp body")
	}
	if string(body) == string(checksum) {
		log.Printf("same version found for %s, skipping\n", ip)
		return true, nil
	}
	return false, nil
}

func provisionBatch(
	ch chan error,
	conf *configuration,
	typ serviceType,
	ips []string,
	checksums map[string][]byte,
) {
	for _, ip := range ips {
		if len(conf.Flags.Limit) > 0 {
			_, exists := conf.Flags.Limit[typ]
			if !exists {
				ch <- nil
				continue
			}
		}
		go func(ip string, typ serviceType) {
			// Check health. If needed, provision, then
			// check health again (on delay)

			// Then check version, and update if needed,
			// then check health again (on delay)
			ok, err := checkHealth(conf, ip, typ)
			if err != nil {
				ch <- errors.Wrapf(err,
					"failed check_health %s", ip)
				return
			}
			log.Println("CHECKED HEALTH", ip)
			if !ok {
				err = provision(conf, ip, typ)
				if err != nil {
					ch <- errors.Wrapf(err,
						"failed provision %s", ip)
					return
				}
				log.Println("PROVISIONED", ip)
				ok, err = checkHealth(conf, ip, typ)
				if err != nil || !ok {
					ch <- errors.Wrapf(err,
						"failed health_check after provision %s", ip)
					return
				}
				log.Println("CHECKED HEALTH (AFTER PROV)", ip)
			}
			chk := checksums[conf.Services[typ].VersionCheckDir]
			ul := conf.Services[typ].VersionCheckURL
			ok, err = checkVersion(conf, ip, typ, ul, chk)
			if err != nil {
				ch <- errors.Wrapf(err,
					"failed check_version %s", ip)
				return
			}
			log.Println("CHECKED VERSION", ip)
			if ok {
				ch <- nil
				return
			}
			err = update(conf, ip, typ)
			log.Println("UPDATED", ip)
			ch <- errors.Wrap(err, "update")
		}(ip, typ)
	}
}

func parseFlags() (flags, error) {
	env := flag.String("e", "", "environment")
	upfile := flag.String("u", "Upfile.toml", "path to upfile")
	dry := flag.Bool("d", false, "dry run")
	verbose := flag.Bool("v", false, "verbose output")
	force := flag.Bool("f", false, "force provision")
	limit := flag.String("l", "",
		"limit provision and starting to specific services")
	flag.Parse()
	if *env == "" {
		return flags{}, errors.New("missing environment flag (-e)")
	}
	lim := map[serviceType]struct{}{}
	lims := strings.Split(*limit, ",")
	if len(lims) > 0 && lims[0] != "" {
		for _, service := range lims {
			lim[serviceType(service)] = struct{}{}
		}
	}
	flgs := flags{
		Env:     *env,
		Upfile:  *upfile,
		Dry:     *dry,
		Verbose: *verbose,
		Limit:   lim,
		Force:   *force,
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

func calcChecksums(conf map[serviceType]*serviceConfig) (map[string][]byte, error) {
	chks := map[string][]byte{}
	for typ, service := range conf {
		if service.VersionCheckURL == "" {
			if service.VersionCheckDir != "" {
				log.Printf("warn: VersionCheckDir defined on %s, but missing VersionCheckURL", typ)
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
			chks[dir] = checksum
		}
	}
	return chks, nil
}

func addSelf(c *configuration, ip string) *selfConfig {
	sc := &selfConfig{
		Config: c,
		Self:   struct{ IP string }{IP: ip},
	}
	return sc
}
