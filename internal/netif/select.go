// SPDX-License-Identifier: GPL-3.0-or-later

package netif

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yniverz/slipmat/internal/prolink"
)

// Sighting is a Pro DJ Link device heard during a probe.
type Sighting struct {
	Name   string
	Device uint8
	Type   prolink.DeviceType
	IP     netip.Addr
}

// Probe listens on UDP 50000 for d and returns, per interface name, the
// devices whose keep-alives arrived from that interface's subnet.
func Probe(ctx context.Context, ifs []Interface, d time.Duration) (map[string][]Sighting, error) {
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("0.0.0.0:%d", prolink.PortAnnounce))
	if err != nil {
		if errors.Is(err, syscall.EADDRINUSE) {
			return nil, fmt.Errorf("%w: UDP %d%s", ErrPortInUse, prolink.PortAnnounce, HolderHint("udp", prolink.PortAnnounce))
		}
		return nil, err
	}
	defer pc.Close()
	deadline := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	pc.SetReadDeadline(deadline)
	out := map[string][]Sighting{}
	seen := map[string]bool{}
	buf := make([]byte, 2048)
	for {
		n, from, err := pc.ReadFrom(buf)
		if err != nil {
			break // deadline
		}
		a, err := prolink.DecodeAnnounce(buf[:n])
		if err != nil || a.Kind != prolink.KindKeepAlive {
			continue
		}
		src, _ := netip.AddrFromSlice(from.(*net.UDPAddr).IP.To4())
		for _, ifc := range ifs {
			if !ifc.Prefix.Masked().Contains(src) || ifc.Prefix.Addr() == src {
				continue
			}
			key := ifc.Name + "/" + src.String() + "/" + strconv.Itoa(int(a.DeviceNumber))
			if seen[key] {
				continue
			}
			seen[key] = true
			out[ifc.Name] = append(out[ifc.Name], Sighting{Name: a.Name, Device: a.DeviceNumber, Type: a.DeviceType, IP: src})
		}
	}
	return out, nil
}

// Config is persisted between runs.
type Config struct {
	Interface string `json:"interface,omitempty"`
}

// ConfigPath returns the per-user config file path.
func ConfigPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "slipmat", "config.json"), nil
}

// LoadConfig reads the saved config; a missing file yields an empty config.
func LoadConfig() Config {
	var c Config
	p, err := ConfigPath()
	if err != nil {
		return c
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return c
	}
	json.Unmarshal(b, &c)
	return c
}

// SaveConfig writes the config file.
func SaveConfig(c Config) error {
	p, err := ConfigPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return os.WriteFile(p, append(b, '\n'), 0o644)
}

// Chooser picks the interface to use.
type Chooser struct {
	Spec        string        // --interface value ("" = auto)
	Interactive bool          // stdin is a terminal
	In          io.Reader     // menu input
	Out         io.Writer     // menu output
	ProbeFor    time.Duration // how long to listen for traffic
	Log         func(format string, args ...any)
}

// Choose resolves the interface: explicit spec > saved choice (if still
// present) > the only interface with Pro DJ Link traffic > interactive menu.
// A menu choice is saved for next time.
func (c *Chooser) Choose(ctx context.Context) (Interface, error) {
	ifs, err := List()
	if err != nil {
		return Interface{}, err
	}
	if len(ifs) == 0 {
		return Interface{}, errors.New("no usable network interface with an IPv4 address (is the Ethernet cable plugged in?)")
	}
	if c.Spec != "" {
		return Find(ifs, c.Spec)
	}
	if saved := LoadConfig().Interface; saved != "" {
		if ifc, err := Find(ifs, saved); err == nil {
			c.Log("using saved interface %s (change with --interface or --pick)", ifc)
			return ifc, nil
		}
		c.Log("saved interface %q is not available; choosing again", saved)
	}
	c.Log("listening %s for Pro DJ Link devices on all interfaces...", c.ProbeFor)
	seen, err := Probe(ctx, ifs, c.ProbeFor)
	if err != nil {
		return Interface{}, err
	}
	var withTraffic []Interface
	for _, ifc := range ifs {
		if len(seen[ifc.Name]) > 0 {
			withTraffic = append(withTraffic, ifc)
		}
	}
	if len(withTraffic) == 1 {
		ifc := withTraffic[0]
		c.Log("found Pro DJ Link devices on %s: %s", ifc, describeSightings(seen[ifc.Name]))
		c.save(ifc)
		return ifc, nil
	}
	if !c.Interactive {
		return Interface{}, errors.New("cannot choose a network interface automatically; pass --interface (see `slipmat interfaces`)")
	}
	ifc, err := Menu(c.In, c.Out, ifs, seen)
	if err != nil {
		return Interface{}, err
	}
	c.save(ifc)
	return ifc, nil
}

func (c *Chooser) save(ifc Interface) {
	if err := SaveConfig(Config{Interface: ifc.Name}); err != nil {
		c.Log("could not save interface choice: %v", err)
	}
}

func describeSightings(s []Sighting) string {
	var parts []string
	for _, x := range s {
		parts = append(parts, fmt.Sprintf("%s #%d (%s, %s)", x.Name, x.Device, x.Type, x.IP))
	}
	return strings.Join(parts, ", ")
}

// Table renders interfaces with probe results.
func Table(w io.Writer, ifs []Interface, seen map[string][]Sighting) {
	for i, ifc := range ifs {
		label := ifc.Label
		if label == "" {
			label = "-"
		}
		notes := []string{}
		if ifc.Wireless {
			notes = append(notes, "wireless: not recommended")
		}
		if ifc.LinkLocal {
			notes = append(notes, "link-local")
		}
		if s := seen[ifc.Name]; len(s) > 0 {
			notes = append(notes, "Pro DJ Link: "+describeSightings(s))
		}
		fmt.Fprintf(w, "  %d) %-6s %-28s %-18s %s\n", i+1, ifc.Name, label, ifc.Prefix, strings.Join(notes, "; "))
	}
}

// Menu asks the user to pick an interface by number.
func Menu(in io.Reader, out io.Writer, ifs []Interface, seen map[string][]Sighting) (Interface, error) {
	fmt.Fprintln(out, "Which network interface are the CDJs connected to?")
	Table(out, ifs, seen)
	r := bufio.NewReader(in)
	for {
		fmt.Fprintf(out, "Enter 1-%d: ", len(ifs))
		line, err := r.ReadString('\n')
		n, convErr := strconv.Atoi(strings.TrimSpace(line))
		if convErr == nil && n >= 1 && n <= len(ifs) {
			return ifs[n-1], nil
		}
		if err != nil {
			return Interface{}, errors.New("no interface chosen")
		}
	}
}

// Guess picks the most likely CDJ interface without asking: the saved
// choice if still present, else the only interface with Pro DJ Link
// traffic, else the only wired interface. Used when the probe can't run
// (e.g. rekordbox holds port 50000).
func Guess(ifs []Interface, seen map[string][]Sighting, saved string) (Interface, error) {
	if saved != "" {
		if ifc, err := Find(ifs, saved); err == nil {
			return ifc, nil
		}
	}
	var traffic, wired []Interface
	for _, ifc := range ifs {
		if len(seen[ifc.Name]) > 0 {
			traffic = append(traffic, ifc)
		}
		if !ifc.Wireless {
			wired = append(wired, ifc)
		}
	}
	if len(traffic) == 1 {
		return traffic[0], nil
	}
	if len(traffic) == 0 && len(wired) == 1 {
		return wired[0], nil
	}
	return Interface{}, errors.New("cannot guess the CDJ interface; pass it explicitly (see `slipmat interfaces`)")
}
