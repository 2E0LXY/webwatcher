package agent

import (
	"fmt"
	"net"
)

// InstancePort is the loopback port held while an agent runs. Holding it is the
// single-instance lock: it is released by the operating system if the process dies.
const InstancePort = 47831

// AcquireSingleInstance returns a release function, or an error if another agent holds the lock.
func AcquireSingleInstance(port int) (release func(), err error) {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, fmt.Errorf("another webwatcher-agent is already running on this PC")
	}
	return func() { l.Close() }, nil
}
