// up ensures a project's servers are deployed successfully in one command.
package main

import (
	"flag"
	"log"
	"math/rand"
	"os"
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

	// Limit the changed services to those enumerated if the flag is
	// provided
	Limit map[up.InvName]struct{}

	// Vars passed into `up` at runtime to be used in start commands.
	Vars map[string]string
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
	log.Printf("\n\nupfile conf\n%+v\n", conf)
}

func parseFlags() (flags, error) {
	f := flag.NewFlagSet("flags", flag.ExitOnError)
	upfile := f.String("u", "Upfile", "path to upfile")
	limit := f.String("l", "", "limit to specific services")
	vars := f.String("x", "", "comma-separated extra vars for commands, "+
		"e.g. Color=Red,Font=Small")
	f.Parse(os.Args)
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
		Upfile: *upfile,
		Limit:  lim,
		Vars:   extraVars,
	}
	return flgs, nil
}

/*
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
	batches, err := makeBatches(conf.Services, conf.Flags.Limit)
	if err != nil {
		errLog.Fatal(errors.Wrap(err, "make batches"))
	}
	if conf.Flags.Verbose {
		log.Printf("got batches: %s\n", batches)
	}

	// checksums maps each filepath to a sha256 checksum
	log.Printf("calculating checksums...\n\n")
	checksums, err := calcChecksums(conf.Services, conf.Flags.Limit)
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
		if info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		files = append(files, pth)
		return nil
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

func makeBatches(
	conf map[serviceType]*serviceConfig,
	limit map[serviceType]struct{},
) (batch, error) {
	batches := batch{}
	for typ, service := range conf {
		if len(limit) > 0 {
			if _, exist := limit[typ]; !exist {
				continue
			}
		}
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
	limit map[serviceType]struct{},
) (map[string]string, error) {
	chks := map[string]string{}
	for typ, service := range conf {
		if len(limit) > 0 {
			if _, exist := limit[typ]; !exist {
				continue
			}
		}
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
*/
