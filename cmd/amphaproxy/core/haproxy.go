package core

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	amp "github.com/appcelerator/amp.stackv13/config"
	"github.com/appcelerator/amp/pkg/labels"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"golang.org/x/net/context"
)

const (
	dockerStackLabelName = "com.docker.stack.namespace"
	tcpLabel             = "tcp"
)

//HAProxy haproxy struct
type HAProxy struct {
	docker             *client.Client
	mappingMap         map[string]*publicMapping
	exec               *exec.Cmd
	isLoadingConf      bool
	dnsRetryLoopID     int
	dnsNotResolvedList []string
	updateID           int
	updateChannel      chan int
}

type publicMapping struct {
	stack   string //Stack name, used in the url host part
	service string //service to reach
	label   string //label used in the url host part
	port    string //internal service port
	mode    string //only for tcp mode (grpc)
	portTo  string //internal grpc port
}

var (
	haproxy HAProxy
	ctx     context.Context
)

//Set app mate initial values
func (app *HAProxy) init() {
	app.mappingMap = make(map[string]*publicMapping)
	ctx = context.Background()
	if err := dockerInit(); err != nil {
		fmt.Printf("Init error: %v\n", err)
		os.Exit(1)
	}
	app.updateChannel = make(chan int)
	app.isLoadingConf = false
	app.dnsNotResolvedList = []string{}
	haproxy.updateConfiguration(true)
}

func dockerInit() error {
	// Connection to Docker
	defaultHeaders := map[string]string{"User-Agent": "haproxy"}
	cli, err := client.NewClient(amp.DockerDefaultURL, amp.DockerDefaultVersion, nil, defaultHeaders)
	if err != nil {
		return err
	}
	haproxy.docker = cli
	fmt.Println("Connected to Docker-engine")
	return nil
}

//Launch a routine to catch SIGTERM Signal
func (app *HAProxy) trapSignal() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Println("\namp-haproxy-controller received SIGTERM signal")
		app.docker.Close()
		os.Exit(1)
	}()
}

//Launch HAProxy using cmd command
func (app *HAProxy) start() {
	//launch HAPRoxy
	go func() {
		fmt.Println("launching HAProxy on initial configuration")
		app.exec = exec.Command("haproxy", "-f", "/usr/local/etc/haproxy/haproxy.cfg")
		app.exec.Stdout = os.Stdout
		app.exec.Stderr = os.Stderr
		err := app.exec.Run()
		if err != nil {
			fmt.Printf("HAProxy exit with error: %v\n", err)
			app.docker.Close()
			os.Exit(1)
		}
	}()
	//Launch main docker watch loop
	app.dockerWatch()
	//Launch main haproxy update loop
	go func() {
		for {
			uid := <-app.updateChannel
			if uid == app.updateID {
				app.updateConfiguration(true)
			}
		}
	}()
}

//Stop HAProxy
func (app *HAProxy) stop() {
	fmt.Println("Send SIGTERM signal to HAProxy")
	if app.exec != nil {
		app.exec.Process.Kill()
	}
}

func (app *HAProxy) dockerWatch() {
	go func() {
		for {
			time.Sleep(time.Duration(conf.dockerWatchPeriod) * time.Second)
			if app.updateServiceMap() {
				app.updateID++
				app.updateChannel <- app.updateID
			}
		}
	}()
}

func (app *HAProxy) updateServiceMap() bool {
	list, err := app.docker.ServiceList(ctx, types.ServiceListOptions{})
	if err != nil || len(list) == 0 {
		fmt.Printf("Error reading docker service list: %v\n", err)
		return false
	}
	isItemUpdated := false
	for _, serv := range list {
		if serv.Spec.Labels != nil {
			stackName, exist := serv.Spec.Labels[dockerStackLabelName]
			if !exist {
				stackName = "default"
			}
			serviceName := serv.Spec.Annotations.Name
			mappings, ok := serv.Spec.Labels[labels.LabelsNameMapping]
			if ok {
				if app.addMappings(stackName, serviceName, mappings) {
					isItemUpdated = true
				}
			}
		}

	}
	return isItemUpdated
}

func (app *HAProxy) addMappings(stackName string, serviceName string, mappings string) bool {
	isItemUpdated := false
	mappingList := strings.Split(mappings, ",")
	for _, value := range mappingList {
		if data, err := evalMappingString(strings.Trim(value, " ")); err != nil {
			fmt.Printf("Mapping error for service %s: %v\n", serviceName, err)
		} else {
			mappingID := fmt.Sprintf("%s-%s:%s", stackName, serviceName, data[1])
			mapping, exist := app.mappingMap[mappingID]
			if exist {
				if mapping.label != data[0] || mapping.port != data[1] || mapping.mode != data[2] || mapping.portTo != data[3] {
					exist = false
				}
			}
			if !exist {
				isItemUpdated = true
				mapping := &publicMapping{
					stack:   stackName,
					service: serviceName,
					label:   data[0],
					port:    data[1],
					mode:    data[2],
					portTo:  data[3],
				}
				app.mappingMap[mappingID] = mapping
			}
		}
	}
	return isItemUpdated
}

//Launch HAProxy using cmd command
func (app *HAProxy) reloadConfiguration() {
	app.isLoadingConf = true
	fmt.Println("reloading HAProxy configuration")
	if app.exec == nil {
		fmt.Printf("HAProxy is not started yet, waiting for it\n")
		return
	}
	pid := app.exec.Process.Pid
	fmt.Printf("Execute: %s %s %s %s %d\n", "haproxy", "-f", "/usr/local/etc/haproxy/haproxy.cfg", "-sf", pid)
	app.exec = exec.Command("haproxy", "-f", "/usr/local/etc/haproxy/haproxy.cfg", "-sf", fmt.Sprintf("%d", pid))
	app.exec.Stdout = os.Stdout
	app.exec.Stderr = os.Stderr
	go func() {
		err := app.exec.Run()
		app.isLoadingConf = false
		if err == nil {
			fmt.Println("HAProxy configuration reloaded")
			return
		}
		fmt.Printf("HAProxy reload configuration error: %v\n", err)
		os.Exit(1)
	}()
}

// update configuration managing the isUpdatingConf flag
func (app *HAProxy) updateConfiguration(reload bool) error {
	app.dnsRetryLoopID++
	app.dnsNotResolvedList = []string{}
	err := app.updateConfigurationEff(reload)
	if err == nil {
		app.startDNSRevolverLoop(app.dnsRetryLoopID)
	}
	return err
}

//update HAProxy configuration for master regarding ETCD keys values and make HAProxy reload its configuration if reload is true
func (app *HAProxy) updateConfigurationEff(reload bool) error {
	fmt.Println("update HAProxy configuration")
	fileNameTarget := "/usr/local/etc/haproxy/haproxy.cfg"
	fileNameTpt := "/usr/local/etc/haproxy/haproxy.cfg.tpt"
	file, err := os.Create(fileNameTarget + ".new")
	if err != nil {
		fmt.Printf("Error creating new haproxy conffile for creation: %v\n", err)
		return err
	}
	filetpt, err := os.Open(fileNameTpt)
	if err != nil {
		fmt.Printf("Error opening conffile template: %s : %v\n", fileNameTpt, err)
		return err
	}
	scanner := bufio.NewScanner(filetpt)
	skip := false
	for scanner.Scan() {
		line := scanner.Text()
		skip = hasToBeSkipped(line, skip)
		if conf.debug {
			fmt.Printf("line: %t: %s\n", skip, line)
		}
		if !skip {
			if strings.HasPrefix(strings.Trim(line, " "), "[frontendInline]") {
				app.writeFrontendInline(file)
			} else if strings.HasPrefix(strings.Trim(line, " "), "[backends]") {
				app.writeBackend(file)
			} else if strings.HasPrefix(strings.Trim(line, " "), "[frontend]") {
				app.writeFrontend(file)
			} else {
				file.WriteString(line + "\n")
			}
		}
	}
	if err = scanner.Err(); err != nil {
		fmt.Printf("Error reading haproxy conffile template: %s %v\n", fileNameTpt, err)
		file.Close()
		return err
	}
	file.Close()
	os.Remove(fileNameTarget)
	err2 := os.Rename(fileNameTarget+".new", fileNameTarget)
	if err2 != nil {
		fmt.Printf("Error renaming haproxy conffile .new: %v\n", err)
		return err
	}
	fmt.Println("HAProxy configuration updated")
	if reload {
		app.reloadConfiguration()
	}
	return nil
}

// compute if the line is the begining of a block which should be skipped or not
func hasToBeSkipped(line string, skip bool) bool {
	if line == "" {
		//if blanck line then end of skip
		return false
	} else if skip {
		// if skipped mode and line not black then continue to skip
		return true
	}

	ref := strings.Trim(line, " ")

	if strings.HasPrefix(ref, "frontend stack_") || strings.HasPrefix(ref, "backend stack_") {
		//if main mode and stack frontend or backend then skip
		return true
	}
	//if line not "" and not skip then continue to not skip
	return false
}

// write backends for main service configuration
func (app *HAProxy) writeFrontendInline(file *os.File) error {
	for _, mapping := range app.mappingMap {
		if mapping.mode != tcpLabel {
			line := fmt.Sprintf("    use_backend bk_%s_%s-%s if { hdr_beg(host) -i %s.%s. }\n", mapping.stack, mapping.service, mapping.port, mapping.label, mapping.stack)
			file.WriteString(line)
			fmt.Printf(line)
		}
	}
	return nil
}

// write backends for main service configuration
func (app *HAProxy) writeFrontend(file *os.File) error {
	for _, mapping := range app.mappingMap {
		if mapping.mode == tcpLabel {
			lines := []string{
				fmt.Sprintf("\nfrontend %s_%s_grpc\n", mapping.stack, mapping.service),
				"    mode tcp\n",
				fmt.Sprintf("    bind *:%s npn spdy/2 alpn h2,http/1.1\n", mapping.portTo),
				fmt.Sprintf("    default_backend bk_%s_%s-%s\n\n", mapping.stack, mapping.service, mapping.port)}
			for _, line := range lines {
				file.WriteString(line)
				fmt.Printf(line)
			}
		}
	}
	return nil
}

// write backends for stack haproxy configuration
func (app *HAProxy) writeBackend(file *os.File) error {
	for _, mapping := range app.mappingMap {
		dnsResolved := app.tryToResolvDNS(mapping.service)
		line := fmt.Sprintf("\nbackend bk_%s_%s-%s\n", mapping.stack, mapping.service, mapping.port)
		file.WriteString(line)
		fmt.Printf(line)
		if mapping.mode == tcpLabel {
			if dnsResolved {
				line1 := fmt.Sprintf("    mode tcp\n    server %s_1 %s_%s:%s resolvers docker resolve-prefer ipv4\n", mapping.service, mapping.stack, mapping.service, mapping.port)
				file.WriteString(line1)
				fmt.Printf(line1)
			} else {
				line1 := "    #dns name not resolved\n"
				file.WriteString(line1)
				fmt.Printf(line1)
				line2 := fmt.Sprintf("#mode tcp\n#    server %s_1 %s_%s:%s resolvers docker resolve-prefer ipv4\n", mapping.service, mapping.stack, mapping.service, mapping.port)
				file.WriteString(line2)
				fmt.Printf(line2)
				app.addDNSNameInRetryList(mapping.service)
			}
		} else {

			//if dns name is not resolved haproxy (v1.6) won't start or accept the new configuration so server is disabled
			//to be removed when haproxy will fixe this bug
			if dnsResolved {
				line1 := fmt.Sprintf("    server %s_1 %s_%s:%s resolvers docker resolve-prefer ipv4\n", mapping.service, mapping.stack, mapping.service, mapping.port)
				file.WriteString(line1)
				fmt.Printf(line1)
			} else {
				line1 := "    #dns name not resolved\n"
				file.WriteString(line1)
				fmt.Printf(line1)
				line2 := fmt.Sprintf("    #server %s_1 %s_%s:%s resolvers docker resolve-prefer ipv4\n", mapping.service, mapping.stack, mapping.service, mapping.port)
				file.WriteString(line2)
				fmt.Printf(line2)
				app.addDNSNameInRetryList(mapping.service)
			}
		}
	}
	return nil
}

// test if a dns name is resolved or not
func (app *HAProxy) tryToResolvDNS(name string) bool {
	_, err := net.LookupIP(name)
	if err != nil {
		return false
	}
	return true
}

// add unresolved dns name in list to be retested later
func (app *HAProxy) addDNSNameInRetryList(name string) {
	app.dnsNotResolvedList = append(app.dnsNotResolvedList, name)
}

// on regular basis try to see if one of the unresolved dns become resolved, if so execute a configuration update.
// need to have only one loop at a time, if the id change then the current loop should stop
// id is incremented at each configuration update which can be trigger by ETCD wash also
func (app *HAProxy) startDNSRevolverLoop(loopID int) {
	//if no unresolved DNS name then not needed to start the loop
	if len(haproxy.dnsNotResolvedList) == 0 {
		return
	}
	fmt.Printf("Start DNS resolver id: %d\n", loopID)
	go func() {
		for {
			for _, name := range haproxy.dnsNotResolvedList {
				if app.tryToResolvDNS(name) {
					if haproxy.dnsRetryLoopID == loopID {
						fmt.Printf("DNS %s resolved, update configuration\n", name)
						app.updateConfiguration(true)
					}
					fmt.Printf("Stop DNS resolver id: %d\n", loopID)
					return
				}
			}
			time.Sleep(10)
			if haproxy.dnsRetryLoopID != loopID {
				fmt.Printf("Stop DNS resolver id: %d\n", loopID)
				return
			}
		}
	}()
}

// parse a value of io.amp.mapping label
func evalMappingString(mapping string) ([]string, error) {
	ret := make([]string, 4, 4)
	if mapping == "" {
		return ret, fmt.Errorf("mapping is empty")
	}
	data := strings.Split(mapping, ":")
	if len(data) < 2 {
		return ret, fmt.Errorf("mapping format error should be: label:port[:tcp:port]: %s", mapping)
	}
	ret[0] = data[0]
	if _, err := strconv.Atoi(data[1]); err != nil {
		return ret, fmt.Errorf("mapping format error, port should be a number: %s", mapping)
	}
	ret[1] = data[1]
	if len(data) == 3 {
		return ret, fmt.Errorf("mapping format error should be: label:port[:tcp:port]: %s", mapping)
	}
	if len(data) >= 4 {
		if data[2] != "tcp" {
			return ret, fmt.Errorf("mapping format error mode can be only 'tcp' to specify a grpc mapping: %s", mapping)
		}
		ret[2] = data[2]
		if _, err := strconv.Atoi(data[3]); err != nil {
			return ret, fmt.Errorf("mapping format error, internal tcp port should be a number: %s", mapping)
		}
		ret[3] = data[3]

	}
	return ret, nil
}
