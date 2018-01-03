// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"math/rand"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
)

type configServer struct {
	BaseDir     string `toml:"base_dir"`
	IPs         []string
	Provision   []string
	Start       []string
	HealthCheck []string `toml:"health_check"`

	// ip is used internally, so it's unexported
	ip string
}

type flags struct {
	Env           string
	Upfile        string
	Dry           bool
	RollingDeploy bool
	Verbose       bool
	Force         bool
	Limit         map[serviceType]struct{}
}

type serviceType string

type lockfileData map[string]map[string][]serviceType

type configuration struct {
	Services map[serviceType]*configServer
	Flags    flags
	Self     *configServer
}

// TODO: show example Upfiles working across windows and linux dev environments
// in readme
func main() {
	// TODO flags: -e extra-vars file for passing in extra template data
	// without it remaining in source, like a secret API key for a health
	// check, or specific IPs for a blue-green deploy
	//
	// TODO flags: -l limit to specific services, like just restart web,
	// but do healthchecks across the board when finished

	log.SetFlags(0)
	rand.Seed(time.Now().Unix())

	flgs, err := parseFlags()
	if err != nil {
		log.Fatal(errors.Wrap(err, "parse flags"))
	}

	upfileData := map[string]map[serviceType]*configServer{}
	if _, err = toml.DecodeFile(flgs.Upfile, &upfileData); err != nil {
		log.Fatal(errors.Wrap(err, "decode toml"))
	}
	lockfile, ld, err := decodeLockfile("Upfile.lock")
	if err != nil {
		log.Fatal(errors.Wrap(err, "decode lockfile"))
	}
	defer lockfile.Close()

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
	var batch1Size, batch2Size int
	batch1, batch2 := map[string][]serviceType{}, map[string][]serviceType{}
	for typ, service := range conf.Services {
		if len(service.IPs) == 0 {
			log.Fatal(fmt.Errorf("no ips for %s", typ))
		}
		if !conf.Flags.RollingDeploy {
			for _, ip := range service.IPs {
				batch1Size++
				batch1[ip] = append(batch1[ip], serviceType(typ))
			}
			continue
		}
		rnd := rand.Intn(len(service.IPs))
		for i, ip := range service.IPs {
			if i == rnd {
				batch1Size++
				batch1[ip] = append(batch1[ip], serviceType(typ))
			} else {
				batch2Size++
				batch2[ip] = append(batch2[ip], serviceType(typ))
			}
		}
	}
	log.Println("provisioning batch 1: ", batch1)
	ch := make(chan error, batch1Size)
	provisionBatch(ch, conf, batch1, ld[conf.Flags.Env])
	for i := 0; i < batch1Size; i++ {
		if err = <-ch; err != nil {
			log.Fatal(errors.Wrap(err, "provision batch 1"))
		}
	}
	close(ch)
	if batch2Size > 0 {
		log.Println("provisioning batch 2: ", batch2)
		ch = make(chan error, batch2Size)
		provisionBatch(ch, conf, batch2, ld[conf.Flags.Env])
		for i := 0; i < batch2Size; i++ {
			if err = <-ch; err != nil {
				log.Fatal(errors.Wrap(err, "provision batch 2"))
			}
		}
		close(ch)
	}
	if !flgs.Dry {
		if err := updateLockfile(conf, ld, lockfile); err != nil {
			log.Fatal(errors.Wrap(err, "update lockfile"))
		}
	}
	log.Println("success")
}

// provision a machine as a specific class, e.g. "web" or "loadbalancer"
func provision(conf *configuration, ip string, typ serviceType) error {
	for _, cmd := range conf.Services[typ].Provision {
		if err := provisionOne(conf, ip, typ, cmd); err != nil {
			return errors.Wrap(err, "provision")
		}
	}
	return start(conf, ip, typ)
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
	conf.Self = conf.Services[typ]
	conf.Self.ip = ip
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, conf); err != nil {
		return errors.Wrap(err, "execute template")
	}
	cmd = string(buf.Bytes())
	log.Printf("[%s] %s: provision %s: %s\n", conf.Flags.Env, typ, ip, cmd)
	if conf.Flags.Dry {
		return nil
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = conf.Services[typ].BaseDir
	out, err := c.CombinedOutput()
	if err != nil {
		log.Println(string(out))
		return errors.Wrap(err, "run cmd")
	}
	if conf.Flags.Verbose {
		log.Println(string(out))
	}
	return nil
}

func start(
	conf *configuration,
	ip string,
	typ serviceType,
) error {
	for _, cmd := range conf.Services[typ].Start {
		if err := startOne(conf, ip, typ, cmd); err != nil {
			return errors.Wrap(err, "start")
		}
	}
	return checkHealth(conf, ip, typ)
}

func startOne(
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
	if conf.Self == nil {
		conf.Self = conf.Services[typ]
		conf.Self.ip = ip
	}
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, conf); err != nil {
		return errors.Wrap(err, "execute template")
	}
	cmd = string(buf.Bytes())
	log.Printf("[%s] %s: start %s, %s\n", conf.Flags.Env, typ, ip, cmd)
	if conf.Flags.Dry {
		return nil
	}
	c := exec.Command("sh", "-c", cmd)
	c.Dir = conf.Services[typ].BaseDir
	out, err := c.CombinedOutput()
	if err != nil {
		log.Println(string(out))
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
) error {
	tmpl, err := template.New("").Parse(tmplCmd)
	if err != nil {
		return errors.Wrap(err, "parse template")
	}
	var byt []byte
	if conf.Self == nil {
		conf.Self = conf.Services[typ]
		conf.Self.ip = ip
	}
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, conf); err != nil {
		return errors.Wrap(err, "execute template")
	}
	cmd := string(buf.Bytes())
	const iterations = 3
	for i := 0; i < iterations; i++ {
		log.Printf("[%s] %s: check_health %s (%d): %s\n",
			conf.Flags.Env, typ, ip, i+1, cmd)
		if conf.Flags.Dry {
			continue
		}
		c := exec.Command("sh", "-c", cmd)
		c.Dir = conf.Services[typ].BaseDir
		out, err := c.CombinedOutput()
		if err != nil {
			log.Println(string(out))
			return errors.Wrap(err, "run cmd")
		}
		if conf.Flags.Verbose {
			log.Println(string(out))
		}
		if i < iterations-1 {
			time.Sleep(time.Second)
		}
	}
	return nil
}

func checkHealth(
	conf *configuration,
	ip string,
	typ serviceType,
) error {
	for _, cmd := range conf.Services[typ].HealthCheck {
		if err := checkHealthOne(conf, ip, typ, cmd); err != nil {
			return errors.Wrap(err, "check health")
		}
	}
	return nil
}

func provisionBatch(
	ch chan error,
	conf *configuration,
	batch map[string][]serviceType,
	lockfileEnv map[string][]serviceType,
) {
	for ip, types := range batch {
		c := copyConfig(conf)
		if _, exists := lockfileEnv[ip]; exists {
			for _, typ := range types {
				if len(conf.Flags.Limit) > 0 {
					_, exists := conf.Flags.Limit[typ]
					if !exists {
						ch <- nil
						continue
					}
				}
				go func(ip string, typ serviceType) {
					var err error
					if conf.Flags.Force {
						err = provision(c, ip, typ)
					} else {
						err = start(c, ip, typ)
					}
					ch <- errors.Wrapf(err, "start %s", ip)
				}(ip, typ)
			}
			continue
		}
		for _, typ := range types {
			if len(conf.Flags.Limit) > 0 {
				_, exists := conf.Flags.Limit[typ]
				if !exists {
					ch <- nil
					continue
				}
			}
			go func(ip string, typ serviceType) {
				err := provision(c, ip, typ)
				ch <- errors.Wrapf(err, "failed provision %s", ip)
			}(ip, typ)
		}
	}
}

func copyConfig(c *configuration) *configuration {
	c2 := &configuration{
		Flags:    c.Flags,
		Services: map[serviceType]*configServer{},
	}
	for k, v := range c.Services {
		c2.Services[k] = v
	}
	return c2
}

func decodeLockfile(
	filename string,
) (*os.File, map[string]map[string][]serviceType, error) {
	ld := lockfileData{}
	fi, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, nil, errors.Wrap(err, "open lockfile")
	}
	byt, err := ioutil.ReadAll(fi)
	if err != nil {
		fi.Close()
		return nil, nil, errors.Wrap(err, "read lockfile")
	}
	if _, err = toml.Decode(string(byt), &ld); err != nil {
		fi.Close()
		return nil, nil, errors.Wrap(err, "decode lockfile")
	}
	return fi, ld, nil
}

func parseFlags() (flags, error) {
	env := flag.String("e", "", "environment")
	upfile := flag.String("u", "Upfile.toml", "path to upfile")
	dry := flag.Bool("d", false, "dry run")
	rollingDeploy := flag.Bool("r", true, "rolling deploy")
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
		Env:           *env,
		Upfile:        *upfile,
		Dry:           *dry,
		RollingDeploy: *rollingDeploy,
		Verbose:       *verbose,
		Limit:         lim,
		Force:         *force,
	}
	return flgs, nil
}

func updateLockfile(
	conf *configuration,
	ld map[string]map[string][]serviceType,
	fi *os.File,
) error {
	if _, err := fi.Seek(0, 0); err != nil {
		return errors.Wrap(err, "seek")
	}
	if err := fi.Truncate(0); err != nil {
		return errors.Wrap(err, "truncate")
	}
	ld[conf.Flags.Env] = map[string][]serviceType{}
	for typ, service := range conf.Services {
		for _, ip := range service.IPs {
			ld[conf.Flags.Env][ip] = append(ld[conf.Flags.Env][ip], typ)
		}
	}
	if err := toml.NewEncoder(fi).Encode(ld); err != nil {
		return errors.Wrap(err, "encode ips")
	}
	return nil
}

func validateLimits(
	limits map[serviceType]struct{},
	services map[serviceType]*configServer,
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
