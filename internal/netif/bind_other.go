// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !darwin && !linux

package netif

func bindToInterface(fd uintptr, ifc Interface) error { return nil }
