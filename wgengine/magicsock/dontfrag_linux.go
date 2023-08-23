// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build linux && !android

package magicsock

import (
	"net"
	"syscall"

	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

func setDontFragment(pconn nettype.PacketConn, network string, enable bool) (err error) {
	value := syscall.IP_PMTUDISC_DONT
	if enable {
		value = syscall.IP_PMTUDISC_DO
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
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_MTU_DISCOVER, value)
		}
		if network == "udp6" {
			err = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IP_MTU_DISCOVER, value)
		}
	})

	return err
}

func ShouldPMTUD(logf logger.Logf) bool {
	return portableShouldPMTUD(logf)
}
