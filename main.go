// sup ensures a project's servers are deployed successfully in one command.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/BurntSushi/toml"
	"github.com/pkg/errors"
)

type configServer struct {
	IP          string
	Dir         string
	IPs         []string
	Provision   []string
	Start       []string
	HealthCheck string `toml:"health_check"`
}

type lockEntry struct {
	IPs []string `toml:"ips"`
}

func main() {
	// TODO flags: -c concurrent-deploy
	// TODO flags: -v vars for passing in extra template data without it
	// remaining in source, like a secret API key for a health check

	log.SetFlags(0)

	env := flag.String("e", "", "environment")
	dry := flag.Bool("d", false, "dry run")
	flag.Parse()
	if *env == "" {
		log.Fatal("missing environment flag (-e)")
	}

	conf := map[string]map[string]*configServer{}
	if _, err := toml.DecodeFile("Upfile.toml", &conf); err != nil {
		log.Fatal(errors.Wrap(err, "decode toml"))
	}
	lockFile := lockEntry{}
	fi, err := os.OpenFile("Upfile.lock", os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		log.Fatal(err)
	}
	defer fi.Close()
	byt, err := ioutil.ReadAll(fi)
	if err != nil {
		log.Fatal(err)
	}
	_, err = toml.Decode(string(byt), &lockFile)
	if err != nil {
		log.Fatal(errors.Wrap(err, "decode lock"))
	}
	lockFileIPs := map[string]struct{}{}
	for _, ip := range lockFile.IPs {
		lockFileIPs[ip] = struct{}{}
	}
	// For each server type, ensure those IPs have been provisioned. If
	// not, do that now.
	oldServers := []struct {
		IP   string
		Type string
	}{}
	var ips []string
	confEnv, exists := conf[*env]
	if !exists {
		log.Fatal(fmt.Errorf("unknown environment %s, not in Upfile.toml", *env))
	}
	for _, confServer := range confEnv {
		for _, ip := range confServer.IPs {
			ips = append(ips, ip)
		}
	}
	ch := make(chan bool, len(ips))
	for typ, confServer := range confEnv {
		for _, ip := range confServer.IPs {
			if _, exists := lockFileIPs[ip]; exists {
				oldServers = append(oldServers, struct {
					IP   string
					Type string
				}{IP: ip, Type: typ})
				continue
			}
			localConf := map[string]*configServer{}
			for k, v := range confEnv {
				localConf[k] = v
			}
			ok, err := provision(localConf, ip, typ, fi, *dry)
			if err != nil {
				log.Fatal(err)
			}
			if !ok {
				log.Fatal("failed on ip", ip)
			}
		}
	}
	// Find the already provisioned IPs, start them
	for _, srv := range oldServers {
		localConf := map[string]*configServer{}
		for k, v := range confEnv {
			localConf[k] = v
		}
		ok, err := start(localConf, srv.IP, srv.Type, *dry)
		if err != nil {
			log.Fatal(err)
		}
		if !ok {
			log.Fatal("failed on ip", srv.IP)
		}
	}
	if _, err = fi.Seek(0, 0); err != nil {
		log.Fatal(err)
	}
	if err = fi.Truncate(0); err != nil {
		log.Fatal(err)
	}
	err = toml.NewEncoder(fi).Encode(lockEntry{ips})
	if err != nil {
		log.Fatal(err)
	}
	close(ch)
}

// provision a machine as a specific class, e.g. "web" or "loadbalancer"
func provision(
	conf map[string]*configServer,
	ip, typ string,
	fi *os.File,
	dry bool,
) (bool, error) {
	// TODO Replace text.template data with Self/Type.IPs, etc.
	// replace the cmd as if it were a template
	for _, cmd := range conf[typ].Provision {
		tmpl, err := template.New("").Parse(cmd)
		if err != nil {
			return false, errors.Wrap(err, "parse template")
		}
		var byt []byte
		conf["Self"] = conf[typ]
		conf["Self"].IP = ip
		buf := bytes.NewBuffer(byt)
		if err = tmpl.Execute(buf, conf); err != nil {
			return false, errors.Wrap(err, "execute template")
		}
		cmd = string(buf.Bytes())
		log.Printf("%s: provision: %s\n", typ, cmd)
		if !dry {
			c := exec.Command("sh", "-c", cmd)
			c.Dir = conf[typ].Dir
			out, err := c.CombinedOutput()
			if err != nil {
				log.Println(string(out))
				return false, errors.Wrap(err, "run cmd")
			}
			log.Println(string(out))
		}
	}
	_, err := fi.WriteString(fmt.Sprintf("%s = \"%s\"\n", ip, typ))
	if err != nil {
		return false, errors.Wrap(err, "append to lockfile")
	}
	return start(conf, ip, typ, dry)
}

func start(
	conf map[string]*configServer,
	ip, typ string,
	dry bool,
) (bool, error) {
	for _, cmd := range conf[typ].Start {
		tmpl, err := template.New("").Parse(cmd)
		if err != nil {
			return false, errors.Wrap(err, "parse template")
		}
		var byt []byte
		conf["Self"] = conf[typ]
		conf["Self"].IP = ip
		buf := bytes.NewBuffer(byt)
		if err = tmpl.Execute(buf, conf); err != nil {
			return false, errors.Wrap(err, "execute template")
		}
		cmd = string(buf.Bytes())
		log.Printf("%s: start: %s\n", typ, cmd)
		if !dry {
			c := exec.Command("sh", "-c", cmd)
			c.Dir = filepath.Join(".", "ansible")
			out, err := c.CombinedOutput()
			if err != nil {
				log.Println(string(out))
				return false, errors.Wrap(err, "run cmd")
			}
			log.Println(string(out))
		}
	}
	return checkHealth(conf, ip, typ, dry)
}

func checkHealth(
	conf map[string]*configServer,
	ip, typ string,
	dry bool,
) (bool, error) {
	tmpl, err := template.New("").Parse(conf[typ].HealthCheck)
	if err != nil {
		return false, errors.Wrap(err, "parse template")
	}
	var byt []byte
	conf["Self"] = conf[typ]
	conf["Self"].IP = ip
	buf := bytes.NewBuffer(byt)
	if err = tmpl.Execute(buf, conf); err != nil {
		return false, errors.Wrap(err, "execute template")
	}
	cmd := string(buf.Bytes())
	log.Printf("%s: check_health: %s\n", typ, cmd)
	if !dry {
		// TODO ensure it passes 3 times in a row over several seconds
		c := exec.Command("sh", "-c", cmd)
		c.Dir = conf[typ].Dir
		out, err := c.CombinedOutput()
		if err != nil {
			log.Println(string(out))
			return false, errors.Wrap(err, "run cmd")
		}
		log.Println(string(out))
	}
	return true, nil
}
