package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

func lookPathReal(n string) (string, error) { return exec.LookPath(n) }

// Dirs holds where the agent keeps its files.
type Dirs struct {
	Config  string // token, config.json, status.json
	Data    string // profile, log
	Profile string
	Log     string
}

// DefaultDirs follows Windows conventions (%APPDATA% for the token, %LOCALAPPDATA%
// for the profile and log) and the XDG-style user directories elsewhere.
func DefaultDirs(env Env, goos string) Dirs {
	var cfg, data string
	switch goos {
	case "windows":
		cfg = winJoin(orElse(env("APPDATA"), winJoin(orElse(env("USERPROFILE"), `C:\Users\Default`), "AppData", "Roaming")), "webwatcher-agent")
		data = winJoin(orElse(env("LOCALAPPDATA"), winJoin(orElse(env("USERPROFILE"), `C:\Users\Default`), "AppData", "Local")), "webwatcher-agent")
		return Dirs{Config: cfg, Data: data, Profile: winJoin(data, "profile"), Log: winJoin(data, "agent.log")}
	}
	home := env("HOME")
	cfgBase := orElse(env("XDG_CONFIG_HOME"), filepath.Join(home, ".config"))
	dataBase := orElse(env("XDG_DATA_HOME"), filepath.Join(home, ".local", "share"))
	if goos == "darwin" {
		cfgBase = filepath.Join(home, "Library", "Application Support")
		dataBase = cfgBase
	}
	cfg = filepath.Join(cfgBase, "webwatcher-agent")
	data = filepath.Join(dataBase, "webwatcher-agent")
	return Dirs{Config: cfg, Data: data, Profile: filepath.Join(data, "profile"), Log: filepath.Join(data, "agent.log")}
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// HereDirs is DefaultDirs for this machine.
func HereDirs() Dirs { return DefaultDirs(os.Getenv, runtime.GOOS) }
