package server

import (
	"net"
	"syscall"
	"time"
)

// tcpUserTimeout is TCP_USER_TIMEOUT, which package syscall does not define.
const tcpUserTimeout = 0x12

// setUserTimeout is best effort: a kernel without the option still serves.
func setUserTimeout(c *net.TCPConn) {
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	_ = raw.Control(func(fd uintptr) {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpUserTimeout, int(userTimeout/time.Millisecond))
	})
}
