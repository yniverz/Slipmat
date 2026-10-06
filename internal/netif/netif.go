// SPDX-License-Identifier: GPL-3.0-or-later

// Package netif discovers network interfaces suitable for Pro DJ Link,
// opens UDP sockets pinned to one interface, and helps the user pick one.
package netif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"syscall"
)

// Interface is an IPv4-capable network interface.
type Interface struct {
	Name      string       // OS name, e.g. "en13"
	Index     int          // OS interface index
	Label     string       // human label, e.g. "USB 10/100/1G/2.5G LAN" (macOS hardware port)
	MAC       [6]byte      // hardware address (zero if none)
	Prefix    netip.Prefix // chosen IPv4 address with mask
	Broadcast netip.Addr   // directed broadcast address of Prefix
	Wireless  bool         // best guess: Wi-Fi
	LinkLocal bool         // Prefix is 169.254/16
}

func (i Interface) String() string {
	label := i.Label
	if label == "" {
		label = i.Name
	}
	return fmt.Sprintf("%s (%s) %s", i.Name, label, i.Prefix)
}

// List returns up, broadcast-capable, non-loopback interfaces that have an
// IPv4 address. Each interface appears once per IPv4 address.
func List() ([]Interface, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	labels := hardwarePorts()
	var out []Interface
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagBroadcast == 0 {
			continue
		}
		if ignoredName(ifc.Name) {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil {
				continue
			}
			ones, _ := ipn.Mask.Size()
			addr, _ := netip.AddrFromSlice(ip4)
			p := netip.PrefixFrom(addr, ones)
			it := Interface{
				Name:      ifc.Name,
				Index:     ifc.Index,
				Label:     labels[ifc.Name],
				Prefix:    p,
				Broadcast: broadcastOf(p),
				LinkLocal: addr.IsLinkLocalUnicast(),
			}
			copy(it.MAC[:], ifc.HardwareAddr)
			it.Wireless = strings.Contains(strings.ToLower(it.Label), "wi-fi") ||
				strings.Contains(strings.ToLower(it.Label), "wlan") || strings.HasPrefix(ifc.Name, "wl")
			out = append(out, it)
		}
	}
	sort.SliceStable(out, func(a, b int) bool {
		if out[a].Wireless != out[b].Wireless {
			return !out[a].Wireless // wired first
		}
		return out[a].Index < out[b].Index
	})
	return out, nil
}

// ignoredName filters virtual interfaces that can never reach CDJs.
func ignoredName(n string) bool {
	for _, p := range []string{"utun", "awdl", "llw", "bridge", "gif", "stf", "anpi", "ap", "vmnet", "docker", "veth", "virbr", "tailscale", "zt"} {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}

func broadcastOf(p netip.Prefix) netip.Addr {
	a := p.Addr().As4()
	bits := p.Bits()
	for i := 0; i < 4; i++ {
		for b := 0; b < 8; b++ {
			if i*8+b >= bits {
				a[i] |= 0x80 >> b
			}
		}
	}
	return netip.AddrFrom4(a)
}

// hardwarePorts maps device names to macOS hardware-port labels
// ("Wi-Fi", "USB 10/100/1G/2.5G LAN"). Empty on other platforms.
func hardwarePorts() map[string]string {
	m := map[string]string{}
	if runtime.GOOS != "darwin" {
		return m
	}
	out, err := exec.Command("networksetup", "-listallhardwareports").Output()
	if err != nil {
		return m
	}
	var port string
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "Hardware Port: "):
			port = strings.TrimPrefix(line, "Hardware Port: ")
		case strings.HasPrefix(line, "Device: ") && port != "":
			m[strings.TrimPrefix(line, "Device: ")] = port
			port = ""
		}
	}
	return m
}

// Find resolves a user-supplied interface spec: an OS name ("en13"), a
// hardware-port label ("Wi-Fi", case-insensitive substring) or an IPv4
// address on the interface.
func Find(ifs []Interface, spec string) (Interface, error) {
	spec = strings.TrimSpace(spec)
	for _, i := range ifs {
		if i.Name == spec || i.Prefix.Addr().String() == spec {
			return i, nil
		}
	}
	var hits []Interface
	for _, i := range ifs {
		if i.Label != "" && strings.Contains(strings.ToLower(i.Label), strings.ToLower(spec)) {
			hits = append(hits, i)
		}
	}
	if len(hits) == 1 {
		return hits[0], nil
	}
	if len(hits) > 1 {
		return Interface{}, fmt.Errorf("interface %q is ambiguous (%d matches)", spec, len(hits))
	}
	return Interface{}, fmt.Errorf("no usable IPv4 interface matches %q", spec)
}

// ErrPortInUse is returned when a Pro DJ Link port is held by another program.
var ErrPortInUse = errors.New("port in use")

// ListenUDP opens a UDP socket on port (0 = ephemeral), receiving broadcasts
// and pinned to ifc so that sends never leave through another interface.
func ListenUDP(ctx context.Context, ifc Interface, port int) (*net.UDPConn, error) {
	lc := net.ListenConfig{Control: func(network, address string, c syscall.RawConn) error {
		var serr error
		err := c.Control(func(fd uintptr) {
			serr = bindToInterface(fd, ifc)
		})
		if err != nil {
			return err
		}
		return serr
	}}
	pc, err := lc.ListenPacket(ctx, "udp4", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%w: UDP %d%s", ErrPortInUse, port, HolderHint("udp", port))
		}
		return nil, err
	}
	return pc.(*net.UDPConn), nil
}

// HolderHint names the process holding a port, if it can be determined
// (uses lsof; best effort). Returns "" or " (held by rekordbox, pid 123)".
func HolderHint(proto string, port int) string {
	if _, err := exec.LookPath("lsof"); err != nil {
		return ""
	}
	arg := fmt.Sprintf("-i%s:%d", strings.ToUpper(proto), port)
	out, err := exec.Command("lsof", "-nP", "-Fcp", arg).Output()
	if err != nil {
		return ""
	}
	var pid, cmd string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "p") && pid == "" {
			pid = line[1:]
		}
		if strings.HasPrefix(line, "c") && cmd == "" {
			cmd = line[1:]
		}
	}
	if cmd == "" {
		return ""
	}
	hint := fmt.Sprintf(" (held by %s, pid %s", cmd, pid)
	if strings.Contains(strings.ToLower(cmd), "rekordbox") || strings.Contains(cmd, "edb_stream") {
		hint += " — quit rekordbox first"
	}
	return hint + ")"
}
