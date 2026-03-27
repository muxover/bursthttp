package client

import (
	"syscall"
)

const (
	// TCP_FASTOPEN_CONNECT (Linux 4.11+): enables client-side TFO transparently
	// on connect(). This is the correct option for outgoing connections.
	// TCP_FASTOPEN (23) is the server-side option for listen queues — not useful here.
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
