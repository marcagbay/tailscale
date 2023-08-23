// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build darwin && !ios

package magicsock

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

func setDontFragment(pconn nettype.PacketConn, network string, enable bool) (err error) {
	value := 0
	if enable {
		value = 1
	}
	c, ok := pconn.(*net.UDPConn)
	if !ok {
		return nil
	}
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}

	rc.Control(func(fd uintptr) {
		if network == "udp4" {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, unix.IP_DONTFRAG, value)
		}
		if network == "udp6" {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, unix.IPV6_DONTFRAG, value)
		}
	})

	return err
}

func ShouldPMTUD(logf logger.Logf) bool {
	return portableShouldPMTUD(logf)
}
