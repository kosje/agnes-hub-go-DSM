//go:build !linux

package main

import (
	"fmt"
	"net"
)

// buildListeners 在非 Linux 平台（Windows / macOS 单机运行）上退化为单套接字，
// 行为与改造前一致：直接监听给定的 host:port。
// 飞牛 fnOS 部署走的是 listener_linux.go 的双栈实现。
func buildListeners(host, port string) ([]net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return nil, fmt.Errorf("监听失败：%v", err)
	}
	return []net.Listener{ln}, nil
}
