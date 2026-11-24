package agent

import (
	"encoding/xml"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"
)

type recRunner struct {
	calls [][]string
	fail  map[string]error
	out   map[string]string
}

func (r *recRunner) run(name string, args ...string) (string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	if len(args) > 0 {
		return r.out[args[0]], r.fail[args[0]]
	}
	return "", nil
}

func TestTaskXML(t *testing.T) {
	x := TaskXML(`C:\Users\Daz & Co\AppData\webwatcher-agent.exe`, `DESKTOP\Daz`)
	if err := xml.Unmarshal([]byte(strings.Replace(x, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)), new(any)); err != nil {
		t.Fatalf("not well-formed: %v\n%s", err, x)
	}
	for _, want := range []string{"<LogonTrigger>", "<RestartOnFailure>", "<Count>999</Count>", "InteractiveToken", "LeastPrivilege", "<Arguments>run</Arguments>",
		`Daz &amp; Co`, "<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "IgnoreNew", "<UserId>DESKTOP\\Daz</UserId>"} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %q", want)
		}
	}
	for _, bad := range []string{"wwa_", "token", "-token", "HighestAvailable"} {
		if strings.Contains(x, bad) {
			t.Errorf("task XML contains %q", bad)
		}
	}
}

func TestEncodeUTF16(t *testing.T) {
	b := EncodeUTF16("a£😀")
	if b[0] != 0xFF || b[1] != 0xFE || len(b)%2 != 0 {
		t.Fatal("BOM or length")
	}
	var u []uint16
	for i := 2; i < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	if string(utf16.Decode(u)) != "a£😀" {
		t.Fatal("round trip")
	}
}

func TestInstallUninstall(t *testing.T) {
	r := &recRunner{}
	if err := Install("windows", r.run, `C:\x\wwa.exe`, `PC\me`); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || r.calls[0][0] != "schtasks" || r.calls[0][1] != "/Create" || r.calls[0][2] != "/TN" || r.calls[0][3] != TaskName || r.calls[0][4] != "/XML" || r.calls[0][6] != "/F" || r.calls[1][1] != "/Run" {
		t.Fatalf("%v", r.calls)
	}
	for _, c := range r.calls {
		for _, a := range c {
			if strings.Contains(a, "wwa_") || strings.Contains(a, "token") {
				t.Fatalf("token on a command line: %v", c)
			}
		}
	}
	if err := Install("linux", r.run, "x", "u"); err == nil {
		t.Fatal("install on linux")
	}
	r2 := &recRunner{fail: map[string]error{"/Create": errors.New("exit 1")}, out: map[string]string{"/Create": "ERROR: Access is denied."}}
	if err := Install("windows", r2.run, "x", "u"); err == nil || !strings.Contains(err.Error(), "Access is denied") {
		t.Fatalf("%v", err)
	}
	r3 := &recRunner{}
	if err := Uninstall("windows", r3.run); err != nil || len(r3.calls) != 1 || r3.calls[0][1] != "/Delete" || r3.calls[0][2] != "/TN" || r3.calls[0][3] != TaskName {
		t.Fatalf("%v %v", err, r3.calls)
	}
	gone := &recRunner{fail: map[string]error{"/Delete": errors.New("exit 1")}, out: map[string]string{"/Delete": "ERROR: The system cannot find the file specified."}}
	if err := Uninstall("windows", gone.run); err != nil {
		t.Fatalf("missing task is not an error: %v", err)
	}
	denied := &recRunner{fail: map[string]error{"/Delete": errors.New("exit 1")}, out: map[string]string{"/Delete": "ERROR: Access is denied."}}
	if err := Uninstall("windows", denied.run); err == nil {
		t.Fatal("real failure swallowed")
	}
	if TaskInstalled((&recRunner{fail: map[string]error{"/Query": errors.New("x")}}).run) {
		t.Fatal("reported installed")
	}
}
