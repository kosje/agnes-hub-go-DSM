//go:build linux

package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"syscall"
)

// buildListeners 在 Linux（群晖套件的部署目标）上处理双栈监听。
//
// host 为 0.0.0.0 / 空 / :: 时，同时监听 IPv4(0.0.0.0) 与 IPv6(::)：
// 既能局域网 IPv4 访问，也能被外网 IPv6 域名直达。
// IPv6 套接字强制 V6ONLY=1，避免与 IPv4 监听抢占同一端口导致 bind 失败。
// 若某个栈绑定失败，仅记警告并继续（只有两个栈都失败才致命）。
func buildListeners(host, port string) ([]net.Listener, error) {
	if host != "0.0.0.0" && host != "" && host != "::" {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, port))
		if err != nil {
			return nil, err
		}
		return []net.Listener{ln}, nil
	}

	var listeners []net.Listener
	if l, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", port)); err == nil {
		listeners = append(listeners, l)
	} else {
		log.Printf("警告：IPv4(0.0.0.0) 监听失败：%v", err)
	}

	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var sockErr error
			if err := c.Control(func(fd uintptr) {
				// 强制 IPv6 仅监听 v6，IPv4 由上面的 0.0.0.0 负责，互不抢占端口。
				sockErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_V6ONLY, 1)
			}); err != nil {
				return err
			}
			return sockErr
		},
	}
	if l, err := lc.Listen(context.Background(), "tcp", net.JoinHostPort("::", port)); err == nil {
		listeners = append(listeners, l)
	} else {
		log.Printf("警告：IPv6(::) 监听失败：%v", err)
	}

	if len(listeners) == 0 {
		return nil, fmt.Errorf("IPv4 与 IPv6 均无法监听端口 %s", port)
	}
	return listeners, nil
}
