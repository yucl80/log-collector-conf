package main

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"

	"io"
	"github.com/docker/docker/api/types/filters"
	"text/template"
	"os"
	"io/ioutil"
	"strings"
	"syscall"
	"os/signal"
	"path/filepath"
)

var (
	tmpl             string
	host             = ""
	bootstrapServers = ""
	workDataRoot     = "/data/filebeat"
	ignoreOlder      = "172800"
	targetFilename   = "/tmp/conf.d/filebeat.yml"
	templateFilename = "template/conf.gotmpl"
	logBaseTag       = "/mwbase/applogs"
)

type ContainerInfo struct {
	ID          string
	MountSource string
	Stack       string
	Service     string
	Index       string
	Name        string
}

type ContainerChangeEvent struct {
	Info   map[string]*ContainerInfo
	action string
}

type TemplateVars struct {
	ContainerInfoMap map[string]*ContainerInfo
	BootstrapServers string
	Host             string
	SincedbRoot      string
	IgnoreOlder      string
}

func init() {
	b, _ := ioutil.ReadFile("/etc/hostname")
	if len(b) > 0 {
		b = b[0: len(b)-1]
	}
	host = string(b)
}

func main() {
	initSysSignal()
	vDataRoot := os.Getenv("WORK_DATA_ROOT")
	if vDataRoot != "" && vDataRoot != "/" {
		workDataRoot = vDataRoot
	}
	vLogBaseTag := os.Getenv("LOG_BASE_TAG")
	if vLogBaseTag != "" {
		logBaseTag = vLogBaseTag
	}
	vFilename := os.Getenv("CONF_FILENAME")
	if vFilename != "" {
		targetFilename = vFilename
	}
	cleanSincedb := os.Getenv("CLEAN_ALL_SINCEDB")
	if cleanSincedb == "true" {
		fmt.Printf("clean all sincedb data \n")
		removeAllSincedb()
		return
	}
	bootstrapServers = os.Getenv("KAFKA_BOOTSTRAP_SERVERS")
	if bootstrapServers == "" {
		fmt.Printf("kafka bootstrap server is empty,please set env KAFKA_BOOTSTRAP_SERVERS \n")
		return
	}
	vIgnoreOlder := os.Getenv("LOG_IGNORE_OLDER")
	if vIgnoreOlder != "" {
		ignoreOlder = vIgnoreOlder
	}
	c := make(chan ContainerChangeEvent, 1)
	go CreateConfig(c)
	watchContainer(c)

}

func watchContainer(c chan<- ContainerChangeEvent) {
	cli, err := client.NewEnvClient()
	if err != nil {
		panic(err)
	}
	containers, err := cli.ContainerList(context.Background(), types.ContainerListOptions{})
	if err != nil {
		msg := fmt.Sprintf("%s", err.Error())
		fmt.Printf("%s\n", msg)

		apiVersion := strings.Trim(strings.Trim(strings.Split(msg, "server API version:")[1], " "), ")")
		os.Setenv("DOCKER_API_VERSION", apiVersion)
		fmt.Printf("set client api version to %s \n", apiVersion)
		cli, err = client.NewEnvClient()
		if err != nil {
			panic(err)
		}

		containers, err = cli.ContainerList(context.Background(), types.ContainerListOptions{})
		if err != nil {
			panic(err)
		}

	}
	cci := make(map[string]*ContainerInfo)
	for _, container := range containers {
		containerInfo, _ := getContainerInfo(cli, container.ID)
		cci[containerInfo.ID] = containerInfo
	}

	c <- ContainerChangeEvent{
		action: "create",
		Info:   cci,
	}

	ops := types.EventsOptions{
		Filters: filters.NewArgs(),
	}
	ops.Filters.Add("type", "container")
	ops.Filters.Add("event", "create")
	ops.Filters.Add("event", "destroy")
	messages, errs := cli.Events(context.Background(), ops)
loop:
	for {
		select {
		case err := <-errs:
			if err != nil && err != io.EOF {
				fmt.Printf("%s\n", err)
			}

			break loop
		case e := <-messages:
			fmt.Printf("%s\n", e)
			if e.Action == "create" {
				containerInfo, _ := getContainerInfo(cli, e.ID)
				fmt.Printf("%s\n", containerInfo)
				c <- ContainerChangeEvent{
					action: "create",
					Info:   map[string]*ContainerInfo{e.ID: containerInfo},
				}
			} else if e.Action == "destroy" {
				fmt.Printf("%s %s\n", e.ID, "destroy")
				c <- ContainerChangeEvent{
					action: "destroy",
					Info:   map[string]*ContainerInfo{e.ID: nil},
				}
			}

		}
	}

}

func removeAllSincedb() {
	files, _ := filepath.Glob(workDataRoot + "/data/*.*")
	for _, file := range files {
		os.Remove(file)
	}
}

func getContainerInfo(cli *client.Client, containerID string) (*ContainerInfo, error) {
	json, _ := cli.ContainerInspect(context.Background(), containerID)
	var logBase string
	for _, mount := range json.Mounts {
		if mount.Destination == logBaseTag {
			p1 := filepath.Dir(mount.Source)
			p1 = filepath.Dir(p1)
			logBase, _ = filepath.Rel(p1, mount.Source)
			break
		}
	}
	var stackName, serviceName, index string
	stackName = json.Config.Labels["io.rancher.stack.name"]
	if stackName != "" {
		serviceName = json.Config.Labels["io.rancher.stack_service.name"][len(stackName)+1:]
		index = json.Config.Labels["io.rancher.container.name"][len(stackName)+len(serviceName)+2:]
	}
	name := json.ContainerJSONBase.Name[1:]

	return &ContainerInfo{
		ID:          containerID,
		MountSource: logBase,
		Stack:       stackName,
		Service:     serviceName,
		Index:       index,
		Name:        name,
	}, nil

}

func CreateConfig(c <-chan ContainerChangeEvent) {
	//defer Recover()

	if err := getTmplFromFile(); err != nil {
		fmt.Printf("get tmple from file failed: %s\n", err.Error())
	}
	cl := make(map[string]*ContainerInfo)

	for {
		select {
		case ci := <-c:
			if ci.action == "create" {
				for k, v := range ci.Info {
					cl[k] = v
				}
			} else if ci.action == "destroy" {
				for k, _ := range ci.Info {
					delete(cl, k)
				}
			}
			createConfig(cl)
		}
	}
}

func getTmplFromFile() error {
	file, err := os.Open(templateFilename)
	if err != nil {
		return fmt.Errorf("create config file error: %s", err.Error())
	}
	defer file.Close()

	fileContent, err := ioutil.ReadAll(file)
	if err != nil {
		return fmt.Errorf("read from %s error: %s", templateFilename, err.Error())
	}

	tmpl = string(fileContent)
	return nil
}

func createConfig(cl map[string]*ContainerInfo) {
	file, err := os.OpenFile(targetFilename, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0755)
	if err != nil {
		fmt.Printf("create config file error: %s", err.Error())
		return
	}
	defer file.Close()

	t := template.Must(template.New("log").Parse(tmpl))
	vars := TemplateVars{
		ContainerInfoMap: cl,
		BootstrapServers: bootstrapServers,
		Host:             host,
		SincedbRoot:      workDataRoot,
		IgnoreOlder:      ignoreOlder,
	}
	err = t.Execute(file, vars)
	if err != nil {
		fmt.Printf("create conf failed: %s\n", err)
	} else {
		fmt.Printf("create conf success\n")
	}

}

func initSysSignal() {
	sc := make(chan os.Signal, 1)
	signal.Notify(sc,
		syscall.SIGINT,
		syscall.SIGTERM,
		syscall.SIGQUIT,
		syscall.SIGKILL,
	)

	go func() {
		sig := <-sc
		fmt.Printf("receive signal [%d] to exit", sig)
		os.Exit(0)
	}()
}
