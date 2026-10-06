// SPDX-License-Identifier: GPL-3.0-or-later

package netif

import "syscall"

// ipBoundIF is IP_BOUND_IF from <netinet/in.h>: restricts a socket to one
// interface for both sending and receiving (including broadcasts).
const ipBoundIF = 25

func bindToInterface(fd uintptr, ifc Interface) error {
	return syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, ipBoundIF, ifc.Index)
}
