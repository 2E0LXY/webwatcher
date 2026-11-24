package agent

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os/exec"
	"strings"
	"unicode/utf16"
)

// TaskName is the per-user Scheduled Task the installer registers.
const TaskName = "webwatcher-agent"

// TaskXML is the Task Scheduler definition: run at this user's logon, in their
// own interactive session (the browser is headed), restart if it fails, never
// stop it for running long, and start it even on battery.
func TaskXML(exe, user string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.4" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>Webwatcher home agent: reads watched pages in your own browser and reports prices to your Webwatcher server.</Description></RegistrationInfo>
  <Triggers>
    <LogonTrigger><Enabled>true</Enabled><UserId>%[2]s</UserId></LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author"><UserId>%[2]s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec><Command>%[1]s</Command><Arguments>run</Arguments></Exec>
  </Actions>
</Task>
`, esc(exe), esc(user))
}

// EncodeUTF16 returns s as UTF-16 little endian with a byte order mark, the
// encoding schtasks /XML expects.
func EncodeUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 0, 2+2*len(u))
	b = append(b, 0xFF, 0xFE)
	for _, c := range u {
		b = append(b, byte(c), byte(c>>8))
	}
	return b
}

// Runner runs an external command; injected so install, uninstall and status can be tested.
type Runner func(name string, args ...string) (string, error)

// ExecRunner runs a real command.
func ExecRunner(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// TaskCreateArgs, TaskDeleteArgs and TaskQueryArgs are the schtasks command lines.
func TaskCreateArgs(xmlPath string) []string {
	return []string{"/Create", "/TN", TaskName, "/XML", xmlPath, "/F"}
}
func TaskDeleteArgs() []string { return []string{"/Delete", "/TN", TaskName, "/F"} }
func TaskQueryArgs() []string  { return []string{"/Query", "/TN", TaskName} }
func TaskRunArgs() []string    { return []string{"/Run", "/TN", TaskName} }
