// SPDX-License-Identifier: GPL-3.0-or-later

package netif

import (
	"errors"
	"syscall"
)

// bindToInterface uses SO_BINDTODEVICE where permitted. Without
// CAP_NET_RAW it fails with EPERM; we then rely on directed broadcasts and
// source filtering instead.
func bindToInterface(fd uintptr, ifc Interface) error {
	err := syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, ifc.Name)
	if errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
