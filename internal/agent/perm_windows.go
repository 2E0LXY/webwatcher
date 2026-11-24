//go:build windows

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
)

// restrictToUser removes inherited permissions from the file and grants full
// control to the current user only: the Windows equivalent of mode 0600.
func restrictToUser(path string) error {
	u, err := user.Current()
	if err != nil {
		return err
	}
	out, err := exec.Command("icacls", path, "/inheritance:r", "/grant:r", u.Username+":F").CombinedOutput()
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("could not restrict the token file to your account (icacls): %v: %s", err, out)
	}
	return nil
}
