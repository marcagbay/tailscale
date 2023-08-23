// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

//go:build (!linux && !darwin) || android || ios

package magicsock

import (
	"errors"

	"tailscale.com/types/logger"
	"tailscale.com/types/nettype"
)

// setDontFragment sets the dontfragment sockopt on pconn on the platforms that support it,
// for both IPv4 and IPv6.
// (C.f. https://datatracker.ietf.org/doc/html/rfc3542#section-11.2 for IPv6 fragmentation)
func setDontFragment(pconn nettype.PacketConn, network string, value bool) (err error) {
	return errors.New("setting don't fragment bit not supported on this OS; peer path MTU discovery disabled")
}

// ShouldPMTUD returns whether this platform should perform peer path MTU discovery.
func ShouldPMTUD(logf logger.Logf) bool {
	return false
}
