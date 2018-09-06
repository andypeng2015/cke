package cke

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/pkg/errors"
	"golang.org/x/crypto/ssh"
)

const (
	ckeLabelName = "com.cybozu.cke"
)

// ContainerEngine defines interfaces for a container engine.
type ContainerEngine interface {
	// PullImage pulls the image for the named container.
	PullImage(name string) error
	// Run runs the named container as a foreground process.
	Run(name string, binds []Mount, command string) error
	// RunWithInput runs the named container as a foreground process with stdin as a string.
	RunWithInput(name string, binds []Mount, command, input string) error
	// RunSystem runs the named container as a system service.
	RunSystem(name string, opts []string, params, extra ServiceParams) error
	// Exists returns if named container exists.
	Exists(name string) (bool, error)
	// Stop stops the named container.
	Stop(name string) error
	// Kill kills the named container.
	Kill(name string) error
	// Remove removes the named container.
	Remove(name string) error
	// Inspect returns ServiceStatus for the named container.
	Inspect(name []string) (map[string]ServiceStatus, error)
	// VolumeCreate creates a local volume.
	VolumeCreate(name string) error
	// VolumeRemove creates a local volume.
	VolumeRemove(name string) error
	// VolumeExists returns true if the named volume exists.
	VolumeExists(name string) (bool, error)
}

type ckeLabel struct {
	BuiltInParams ServiceParams `json:"builtin"`
	ExtraParams   ServiceParams `json:"extra"`
}

// Docker is an implementation of ContainerEngine.
func Docker(agent Agent) ContainerEngine {
	return docker{agent}
}

type docker struct {
	agent Agent
}

func (c docker) PullImage(name string) error {
	img := Image(name)
	stdout, stderr, err := c.agent.Run("docker image list --format '{{.Repository}}:{{.Tag}}'")
	if err != nil {
		return errors.Wrapf(err, "stdout: %s, stderr: %s", stdout, stderr)
	}

	for _, i := range strings.Split(string(stdout), "\n") {
		if img == i {
			return nil
		}
	}

	stdout, stderr, err = c.agent.Run("docker image pull " + img)
	if err != nil {
		return errors.Wrapf(err, "stdout: %s, stderr: %s", stdout, stderr)
	}
	return nil
}

func (c docker) Run(name string, binds []Mount, command string) error {
	args := []string{
		"docker",
		"run",
		"--rm",
		"--network=host",
		"--uts=host",
	}
	for _, m := range binds {
		o := "rw"
		if m.ReadOnly {
			o = "ro"
		}
		args = append(args, fmt.Sprintf("--volume=%s:%s:%s", m.Source, m.Destination, o))
	}
	args = append(args, Image(name), command)

	_, _, err := c.agent.Run(strings.Join(args, " "))
	return err
}

func (c docker) RunWithInput(name string, binds []Mount, command, input string) error {
	args := []string{
		"docker",
		"run",
		"--rm",
		"-i",
		"--network=host",
		"--uts=host",
	}
	for _, m := range binds {
		o := "rw"
		if m.ReadOnly {
			o = "ro"
		}
		args = append(args, fmt.Sprintf("--volume=%s:%s:%s", m.Source, m.Destination, o))
	}
	args = append(args, Image(name), command)

	return c.agent.RunWithInput(strings.Join(args, " "), input)
}

func (c docker) RunSystem(name string, opts []string, params, extra ServiceParams) error {
	id, err := c.getID(name)
	if err != nil {
		return err
	}
	if len(id) != 0 {
		cmdline := "docker rm " + name
		stderr, stdout, err := c.agent.Run(cmdline)
		if err != nil {
			return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
		}
	}

	args := []string{
		"docker",
		"run",
		"-d",
		"--name=" + name,
		"--read-only",
		"--network=host",
		"--uts=host",
		"--restart=unless-stopped",
	}
	args = append(args, opts...)

	for _, m := range append(params.ExtraBinds, extra.ExtraBinds...) {
		o := "rw"
		if m.ReadOnly {
			o = "ro"
		}
		args = append(args, fmt.Sprintf("--volume=%s:%s:%s", m.Source, m.Destination, o))
	}
	for k, v := range params.ExtraEnvvar {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}
	for k, v := range extra.ExtraEnvvar {
		args = append(args, "-e", fmt.Sprintf("%s=%s", k, v))
	}

	label := ckeLabel{
		BuiltInParams: params,
		ExtraParams:   extra,
	}
	data, err := json.Marshal(label)
	if err != nil {
		return err
	}
	labelFile, err := c.putData(ckeLabelName + "=" + string(data))
	if err != nil {
		return err
	}
	args = append(args, "--label-file="+labelFile)

	args = append(args, Image(name))

	args = append(args, params.ExtraArguments...)
	args = append(args, extra.ExtraArguments...)

	cmdline := strings.Join(args, " ")
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return nil
}

func (c docker) Stop(name string) error {
	cmdline := "docker container stop " + name
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return nil
}

func (c docker) Kill(name string) error {
	cmdline := "docker container kill " + name
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return nil
}

func (c docker) Remove(name string) error {
	cmdline := "docker container rm " + name
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return nil
}

func (c docker) putData(data string) (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	fileName := filepath.Join("/tmp", hex.EncodeToString(b))
	err = c.agent.RunWithInput("tee "+fileName, data)
	if err != nil {
		return "", err
	}
	return fileName, nil
}

func (c docker) getID(name string) (string, error) {
	cmdline := "docker ps -a --no-trunc --filter name=^/" + name + "$ --format {{.ID}}"
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return "", errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return strings.TrimSpace(string(stdout)), nil
}

func (c docker) Exists(name string) (bool, error) {
	id, err := c.getID(name)
	if err != nil {
		return false, err
	}
	return len(id) != 0, nil
}

func isSkippableError(err error, stderr []byte) bool {
	if e, ok := err.(*ssh.ExitError); ok {
		exitStatus := e.ExitStatus()
		if exitStatus != 1 {
			return false
		}
		scanner := bufio.NewScanner(bytes.NewReader(stderr))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if len(line) == 0 {
				continue
			}
			if strings.HasPrefix(line, "Error: No such container: ") {
				continue
			}
			return false
		}
		return true
	}
	return false
}

func (c docker) Inspect(names []string) (map[string]ServiceStatus, error) {
	cmdline := "docker container inspect " + strings.Join(names, " ")
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil && !isSkippableError(err, stderr) {
		return nil, errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}

	var djs []types.ContainerJSON
	err = json.Unmarshal(stdout, &djs)
	if err != nil {
		return nil, err
	}

	statuses := make(map[string]ServiceStatus)
	for _, dj := range djs {
		name := strings.TrimPrefix(dj.Name, "/")

		var params ckeLabel
		label := dj.Config.Labels[ckeLabelName]

		err = json.Unmarshal([]byte(label), &params)
		if err != nil {
			return nil, err
		}
		statuses[name] = ServiceStatus{
			Running:       dj.State.Running,
			Image:         dj.Config.Image,
			BuiltInParams: params.BuiltInParams,
			ExtraParams:   params.ExtraParams,
		}
	}

	return statuses, nil
}

func (c docker) VolumeCreate(name string) error {
	cmdline := "docker volume create " + name
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return nil
}

func (c docker) VolumeRemove(name string) error {
	cmdline := "docker volume remove " + name
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}
	return nil
}

func (c docker) VolumeExists(name string) (bool, error) {
	cmdline := "docker volume list -q"
	stdout, stderr, err := c.agent.Run(cmdline)
	if err != nil {
		return false, errors.Wrapf(err, "cmdline: %s, stdout: %s, stderr: %s", cmdline, stdout, stderr)
	}

	for _, n := range strings.Split(string(stdout), "\n") {
		if n == name {
			return true, nil
		}
	}
	return false, nil
}
