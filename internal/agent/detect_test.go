package agent

import (
	"errors"
	"testing"
)

func envOf(m map[string]string) Env { return func(k string) string { return m[k] } }

func existsIn(paths ...string) func(string) bool {
	set := map[string]bool{}
	for _, p := range paths {
		set[p] = true
	}
	return func(p string) bool { return set[p] }
}

func TestDetectBrowserWindows(t *testing.T) {
	env := envOf(map[string]string{"ProgramFiles": `C:\Program Files`, "ProgramFiles(x86)": `C:\Program Files (x86)`, "LocalAppData": `C:\Users\Daz\AppData\Local`})
	chromePF := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	chrome86 := `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`
	chromeLocal := `C:\Users\Daz\AppData\Local\Google\Chrome\Application\chrome.exe`
	edge86 := `C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`
	edgePF := `C:\Program Files\Microsoft\Edge\Application\msedge.exe`
	cases := []struct {
		name   string
		exists []string
		want   string
	}{
		{"chrome in Program Files", []string{chromePF, edge86}, chromePF},
		{"chrome in Program Files (x86)", []string{chrome86, edge86}, chrome86},
		{"per-user chrome", []string{chromeLocal, edge86}, chromeLocal},
		{"chrome beats edge", []string{edgePF, chromeLocal}, chromeLocal},
		{"edge only (x86)", []string{edge86}, edge86},
		{"edge only (64-bit)", []string{edgePF}, edgePF},
		{"nothing", nil, ""},
		{"unrelated files only", []string{`C:\Program Files\Mozilla Firefox\firefox.exe`}, ""},
	}
	for _, c := range cases {
		got, ok := DetectBrowser("windows", env, existsIn(c.exists...), nil)
		if got != c.want || ok != (c.want != "") {
			t.Errorf("%s: got %q,%v want %q", c.name, got, ok, c.want)
		}
	}
	// missing env vars do not produce bogus root-relative paths
	if p, ok := DetectBrowser("windows", envOf(nil), func(string) bool { return true }, nil); ok {
		t.Errorf("found %q with no environment", p)
	}
	// trailing slash on a variable is tolerated
	env2 := envOf(map[string]string{"ProgramFiles": `C:\Program Files\`})
	if p, ok := DetectBrowser("windows", env2, existsIn(chromePF), nil); !ok || p != chromePF {
		t.Errorf("trailing slash: %q %v", p, ok)
	}
}

func TestDetectBrowserOthers(t *testing.T) {
	look := func(n string) (string, error) {
		if n == "chromium" {
			return "/usr/bin/chromium", nil
		}
		return "", errors.New("not found")
	}
	if p, ok := DetectBrowser("linux", envOf(nil), existsIn("/usr/bin/chromium"), look); !ok || p != "/usr/bin/chromium" {
		t.Fatal(p, ok)
	}
	mac := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if p, ok := DetectBrowser("darwin", envOf(map[string]string{"HOME": "/Users/d"}), existsIn(mac), nil); !ok || p != mac {
		t.Fatal(p, ok)
	}
	if _, ok := DetectBrowser("linux", envOf(nil), existsIn(), look); ok {
		t.Fatal("found a browser that does not exist")
	}
}

func TestDefaultDirs(t *testing.T) {
	w := DefaultDirs(envOf(map[string]string{"APPDATA": `C:\Users\Daz\AppData\Roaming`, "LOCALAPPDATA": `C:\Users\Daz\AppData\Local`}), "windows")
	if w.Config != `C:\Users\Daz\AppData\Roaming\webwatcher-agent` || w.Profile != `C:\Users\Daz\AppData\Local\webwatcher-agent\profile` || w.Log != `C:\Users\Daz\AppData\Local\webwatcher-agent\agent.log` {
		t.Fatalf("%+v", w)
	}
	l := DefaultDirs(envOf(map[string]string{"HOME": "/home/d"}), "linux")
	if l.Config != "/home/d/.config/webwatcher-agent" || l.Profile != "/home/d/.local/share/webwatcher-agent/profile" {
		t.Fatalf("%+v", l)
	}
}
