//go:build !linux

package client

func applySocketOptions(fd uintptr, config *Config) {}
