package libpod

import (
	"bufio"
	"bytes"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/containers/libpod/pkg/inspect"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// HealthCheckStatus represents the current state of a container
type HealthCheckStatus int

const (
	// HealthCheckSuccess means the health worked
	HealthCheckSuccess HealthCheckStatus = iota
	// HealthCheckFailure means the health ran and failed
	HealthCheckFailure HealthCheckStatus = iota
	// HealthCheckContainerStopped means the health check cannot
	// be run because the container is stopped
	HealthCheckContainerStopped HealthCheckStatus = iota
	// HealthCheckContainerNotFound means the container could
	// not be found in local store
	HealthCheckContainerNotFound HealthCheckStatus = iota
	// HealthCheckNotDefined means the container has no health
	// check defined in it
	HealthCheckNotDefined HealthCheckStatus = iota
	// HealthCheckInternalError means somes something failed obtaining or running
	// a given health check
	HealthCheckInternalError HealthCheckStatus = iota
	// HealthCheckDefined means the healthcheck was found on the container
	HealthCheckDefined HealthCheckStatus = iota

	// MaxHealthCheckNumberLogs is the maximum number of attempts we keep
	// in the healtcheck history file
	MaxHealthCheckNumberLogs int = 5
	// MaxHealthCheckLogLength in characters
	MaxHealthCheckLogLength = 500

	// HealthCheckHealthy describes a healthy container
	HealthCheckHealthy string = "healthy"
	// HealthCheckUnhealthy describes an unhealthy container
	HealthCheckUnhealthy string = "unhealthy"
)

// hcWriteCloser allows us to use bufio as a WriteCloser
type hcWriteCloser struct {
	*bufio.Writer
}

// Used to add a closer to bufio
func (hcwc hcWriteCloser) Close() error {
	return nil
}

// HealthCheck verifies the state and validity of the healthcheck configuration
// on the container and then executes the healthcheck
func (r *Runtime) HealthCheck(name string) (HealthCheckStatus, error) {
	container, err := r.LookupContainer(name)
	if err != nil {
		return HealthCheckContainerNotFound, errors.Wrapf(err, "unable to lookup %s to perform a health check", name)
	}
	hcStatus, err := checkHealthCheckCanBeRun(container)
	if err == nil {
		return container.RunHealthCheck()
	}
	return hcStatus, err
}

// RunHealthCheck runs the health check as defined by the container
func (c *Container) RunHealthCheck() (HealthCheckStatus, error) {
	var (
		newCommand []string
		returnCode int
		capture    bytes.Buffer
	)
	hcStatus, err := checkHealthCheckCanBeRun(c)
	if err != nil {
		return hcStatus, err
	}
	hcCommand := c.HealthCheckConfig().Test
	if len(hcCommand) > 0 && hcCommand[0] == "CMD-SHELL" {
		newCommand = []string{"sh", "-c", strings.Join(hcCommand[1:], " ")}
	} else {
		newCommand = hcCommand
	}
	captureBuffer := bufio.NewWriter(&capture)
	hcw := hcWriteCloser{
		captureBuffer,
	}
	streams := new(AttachStreams)
	streams.OutputStream = hcw
	streams.ErrorStream = hcw
	streams.InputStream = os.Stdin
	streams.AttachOutput = true
	streams.AttachError = true
	streams.AttachInput = true

	logrus.Debugf("executing health check command %s for %s", strings.Join(newCommand, " "), c.ID())
	timeStart := time.Now()
	hcResult := HealthCheckSuccess
	hcErr := c.Exec(false, false, []string{}, newCommand, "", "", streams, 0)
	if hcErr != nil {
		hcResult = HealthCheckFailure
		returnCode = 1
	}
	timeEnd := time.Now()
	eventLog := capture.String()
	if len(eventLog) > MaxHealthCheckLogLength {
		eventLog = eventLog[:MaxHealthCheckLogLength]
	}
	hcl := newHealthCheckLog(timeStart, timeEnd, returnCode, eventLog)
	if err := c.updateHealthCheckLog(hcl); err != nil {
		return hcResult, errors.Wrapf(err, "unable to update health check log %s for %s", c.healthCheckLogPath(), c.ID())
	}
	return hcResult, hcErr
}

func checkHealthCheckCanBeRun(c *Container) (HealthCheckStatus, error) {
	cstate, err := c.State()
	if err != nil {
		return HealthCheckInternalError, err
	}
	if cstate != ContainerStateRunning {
		return HealthCheckContainerStopped, errors.Errorf("container %s is not running", c.ID())
	}
	if !c.HasHealthCheck() {
		return HealthCheckNotDefined, errors.Errorf("container %s has no defined healthcheck", c.ID())
	}
	return HealthCheckDefined, nil
}

func newHealthCheckLog(start, end time.Time, exitCode int, log string) inspect.HealthCheckLog {
	return inspect.HealthCheckLog{
		Start:    start.Format(time.RFC3339Nano),
		End:      end.Format(time.RFC3339Nano),
		ExitCode: exitCode,
		Output:   log,
	}
}

// UpdateHealthCheckLog parses the health check results and writes the log
func (c *Container) updateHealthCheckLog(hcl inspect.HealthCheckLog) error {
	healthCheck, err := c.GetHealthCheckLog()
	if err != nil {
		return err
	}
	if hcl.ExitCode == 0 {
		//	set status to healthy, reset failing state to 0
		healthCheck.Status = HealthCheckHealthy
		healthCheck.FailingStreak = 0
	} else {
		if len(healthCheck.Status) < 1 {
			healthCheck.Status = HealthCheckHealthy
		}
		// increment failing streak
		healthCheck.FailingStreak = healthCheck.FailingStreak + 1
		// if failing streak > retries, then status to unhealthy
		if int(healthCheck.FailingStreak) > c.HealthCheckConfig().Retries {
			healthCheck.Status = HealthCheckUnhealthy
		}
	}
	healthCheck.Log = append(healthCheck.Log, hcl)
	if len(healthCheck.Log) > MaxHealthCheckNumberLogs {
		healthCheck.Log = healthCheck.Log[1:]
	}
	newResults, err := json.Marshal(healthCheck)
	if err != nil {
		return errors.Wrapf(err, "unable to marshall healthchecks for writing")
	}
	return ioutil.WriteFile(c.healthCheckLogPath(), newResults, 0700)
}

// HealthCheckLogPath returns the path for where the health check log is
func (c *Container) healthCheckLogPath() string {
	return filepath.Join(filepath.Dir(c.LogPath()), "healthcheck.log")
}

// GetHealthCheckLog returns HealthCheck results by reading the container's
// health check log file.  If the health check log file does not exist, then
// an empty healthcheck struct is returned
func (c *Container) GetHealthCheckLog() (inspect.HealthCheckResults, error) {
	var healthCheck inspect.HealthCheckResults
	if _, err := os.Stat(c.healthCheckLogPath()); os.IsNotExist(err) {
		return healthCheck, nil
	}
	b, err := ioutil.ReadFile(c.healthCheckLogPath())
	if err != nil {
		return healthCheck, errors.Wrapf(err, "failed to read health check log file %s", c.healthCheckLogPath())
	}
	if err := json.Unmarshal(b, &healthCheck); err != nil {
		return healthCheck, errors.Wrapf(err, "failed to unmarshal existing healthcheck results in %s", c.healthCheckLogPath())
	}
	return healthCheck, nil
}
