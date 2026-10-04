//go:build !linux

package server

import "net"

// setUserTimeout does nothing: only Linux has a TCP user timeout to set.
func setUserTimeout(*net.TCPConn) {}
