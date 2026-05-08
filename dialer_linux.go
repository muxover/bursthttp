package client

import (
	"syscall"
)

const (
	// 30 = TCP_FASTOPEN_CONNECT (Linux 4.11+); 23 is the server-side listen option, not this
	tcpFastOpenConnect = 30
	soReusePort        = 15
)

func applySocketOptions(fd uintptr, config *Config) {
	if config.TCPFastOpen {
		_ = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, tcpFastOpenConnect, 1)
	}
	if config.TCPReusePort {
		_ = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, soReusePort, 1)
	}
}
