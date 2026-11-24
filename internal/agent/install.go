package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Install registers the per-user Scheduled Task and starts it. The task runs
// "<exe> run" at this user's logon and restarts it if it fails. The token is
// never part of the task: the agent reads it from its protected file.
func Install(goos string, run Runner, exe, user string) error {
	if goos != "windows" {
		return fmt.Errorf("install is for Windows (it registers a Scheduled Task); on %s run the agent from your service manager of choice", goos)
	}
	f, err := os.CreateTemp("", "webwatcher-agent-*.xml")
	if err != nil {
		return err
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.Write(EncodeUTF16(TaskXML(exe, user))); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if out, err := run("schtasks", TaskCreateArgs(path)...); err != nil {
		return fmt.Errorf("schtasks could not create the task: %v: %s", err, out)
	}
	if out, err := run("schtasks", TaskRunArgs()...); err != nil {
		return fmt.Errorf("the task was created but could not be started (it will start at your next logon): %v: %s", err, out)
	}
	return nil
}

// Uninstall removes the Scheduled Task. A task that is already gone is not an error.
func Uninstall(goos string, run Runner) error {
	if goos != "windows" {
		return fmt.Errorf("uninstall is for Windows")
	}
	out, err := run("schtasks", TaskDeleteArgs()...)
	if err != nil && !strings.Contains(strings.ToLower(out), "cannot find") && !strings.Contains(strings.ToLower(out), "does not exist") {
		return fmt.Errorf("schtasks could not delete the task: %v: %s", err, out)
	}
	return nil
}

// TaskInstalled reports whether the Scheduled Task exists.
func TaskInstalled(run Runner) bool {
	_, err := run("schtasks", TaskQueryArgs()...)
	return err == nil
}

// ExePath is the absolute path of the running executable.
func ExePath() (string, error) {
	p, err := os.Executable()
	if err != nil {
		return "", err
	}
	p, err = filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if q, err := filepath.EvalSymlinks(p); err == nil {
		p = q
	}
	return p, nil
}

// Here is the running platform.
var Here = runtime.GOOS
