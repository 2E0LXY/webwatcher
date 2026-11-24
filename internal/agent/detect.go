package agent

import (
	"os"
	"runtime"
	"strings"
)

// Env reads an environment variable.
type Env func(string) string

// DetectBrowser finds an installed Chrome or Edge. exists is injected so the
// search can be tested without a real file system. Chrome is preferred; Edge,
// which ships with Windows, is the fallback.
func DetectBrowser(goos string, env Env, exists func(string) bool, lookPath func(string) (string, error)) (string, bool) {
	for _, p := range browserCandidates(goos, env, lookPath) {
		if p != "" && exists(p) {
			return p, true
		}
	}
	return "", false
}

func winJoin(parts ...string) string { return strings.Join(parts, `\`) }

func browserCandidates(goos string, env Env, lookPath func(string) (string, error)) []string {
	var out []string
	switch goos {
	case "windows":
		var roots []string
		for _, k := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if v := strings.TrimRight(env(k), `\/`); v != "" {
				roots = append(roots, v)
			}
		}
		for _, r := range roots {
			out = append(out, winJoin(r, "Google", "Chrome", "Application", "chrome.exe"))
		}
		for _, r := range roots {
			out = append(out, winJoin(r, "Microsoft", "Edge", "Application", "msedge.exe"))
		}
	case "darwin":
		bases := []string{"/Applications"}
		if home := env("HOME"); home != "" {
			bases = append(bases, home+"/Applications")
		}
		for _, base := range bases {
			out = append(out, base+"/Google Chrome.app/Contents/MacOS/Google Chrome")
		}
		for _, base := range bases {
			out = append(out, base+"/Microsoft Edge.app/Contents/MacOS/Microsoft Edge")
		}
	default:
		for _, n := range []string{"google-chrome", "google-chrome-stable", "microsoft-edge", "microsoft-edge-stable", "chromium", "chromium-browser"} {
			if lookPath != nil {
				if p, err := lookPath(n); err == nil {
					out = append(out, p)
				}
			}
		}
	}
	return out
}

// DetectBrowserHere is DetectBrowser against the real machine.
func DetectBrowserHere() (string, bool) {
	exists := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && !st.IsDir()
	}
	return DetectBrowser(runtime.GOOS, os.Getenv, exists, lookPathReal)
}
