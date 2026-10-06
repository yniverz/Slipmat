// SPDX-License-Identifier: GPL-3.0-or-later

// Package monitor decodes and logs Pro DJ Link traffic. The same Observer is
// fed by live sockets (`slipmat monitor`) and by capture files
// (`slipmat decode`), so both produce identical output.
package monitor

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/prolink"
)

// PeerTimeout is how long a device may stay silent before it's considered gone.
const PeerTimeout = 10 * time.Second

// Peer is a device seen sending keep-alives.
type Peer struct {
	Name     string
	Device   uint8
	Type     prolink.DeviceType
	IP       netip.Addr
	MAC      [6]byte
	Class    uint8
	Tail     []byte
	LastSeen time.Time
	Status   string // latest CDJ status summary
}

// Observer decodes packets and maintains a peer table.
type Observer struct {
	// Verbose logs every packet at info (for offline decoding). When false,
	// high-rate packets (status, beats, positions) are logged only when
	// their interesting fields change, and in full at trace level.
	Verbose bool
	// Self is our own address (ignored), if any.
	Self netip.Addr

	log *slog.Logger

	mu       sync.Mutex
	peers    map[peerKey]*Peer
	last     map[string]string // dedupe key -> last summary
	unknown  map[string]bool   // (port,kind) already warned
	counts   map[string]int    // per (port, kind name)
	lastTime time.Time
}

type peerKey struct {
	ip  netip.Addr
	dev uint8
}

// NewObserver returns an Observer logging through log.
func NewObserver(log *slog.Logger) *Observer {
	return &Observer{
		log:     log,
		peers:   map[peerKey]*Peer{},
		last:    map[string]string{},
		unknown: map[string]bool{},
		counts:  map[string]int{},
	}
}

// Packet processes one UDP payload sent from src to dst at time t.
func (o *Observer) Packet(t time.Time, src, dst netip.AddrPort, payload []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.lastTime = t
	if o.Self.IsValid() && src.Addr() == o.Self {
		return
	}
	port := dst.Port()
	route := fmt.Sprintf("%s -> %s", src, dst)
	kind, err := prolink.Kind(payload)
	if err != nil {
		o.log.Warn("malformed packet", "route", route, "port", port, "err", err, "hex", logx.Dump(payload))
		return
	}
	desc, err := prolink.Describe(port, payload)
	if err != nil {
		o.log.Warn("undecodable packet", "route", route, "port", port, "kind", fmt.Sprintf("%#02x", kind), "err", err, "hex", logx.Dump(payload))
		return
	}
	name := kindName(port, kind)
	o.counts[fmt.Sprintf("%d/%s", port, name)]++
	if strings.HasPrefix(name, "unknown") {
		key := fmt.Sprintf("%d/%02x", port, kind)
		if !o.unknown[key] {
			o.unknown[key] = true
			o.log.Warn("unknown packet kind (first sighting)", "route", route, "desc", desc, "hex", logx.Dump(payload))
			return
		}
	}

	switch port {
	case prolink.PortAnnounce:
		o.announce(t, route, desc, payload)
		return
	case prolink.PortStatus:
		if kind == prolink.KindCDJStatus {
			o.cdjStatus(t, src, route, payload)
			return
		}
		if kind == prolink.KindMixerStatus || kind == prolink.KindMixerStatusNew {
			o.dedupe(route+"/mixer", desc, desc, route, payload)
			return
		}
	case prolink.PortBeat:
		if kind == prolink.KindBeat || kind == prolink.KindAbsPosition {
			o.emitTrace(desc, route, payload)
			return
		}
	}
	o.emit(desc, route, payload)
}

func kindName(port uint16, k uint8) string {
	switch port {
	case prolink.PortAnnounce:
		return prolink.AnnounceKindName(k)
	case prolink.PortBeat:
		return prolink.BeatKindName(k)
	case prolink.PortStatus:
		return prolink.StatusKindName(k)
	}
	return fmt.Sprintf("kind-%#02x", k)
}

// emit logs a packet at info (with hex at trace).
func (o *Observer) emit(desc, route string, payload []byte) {
	if logx.TraceEnabled() {
		o.log.Info(desc, "route", route, "hex", logx.Dump(payload))
		return
	}
	o.log.Info(desc, "route", route)
}

// emitTrace logs high-rate packets: info when Verbose, otherwise trace only.
func (o *Observer) emitTrace(desc, route string, payload []byte) {
	if o.Verbose {
		o.emit(desc, route, payload)
		return
	}
	if logx.TraceEnabled() {
		logx.Trace(o.log, desc, "route", route, "hex", logx.Dump(payload))
	}
}

// dedupe logs at info when summary changed for key, else at trace.
func (o *Observer) dedupe(key, summary, desc, route string, payload []byte) {
	if o.Verbose || o.last[key] != summary {
		o.last[key] = summary
		o.emit(desc, route, payload)
		return
	}
	if logx.TraceEnabled() {
		logx.Trace(o.log, desc, "route", route, "hex", logx.Dump(payload))
	}
}

func (o *Observer) announce(t time.Time, route, desc string, payload []byte) {
	a, _ := prolink.DecodeAnnounce(payload)
	if a == nil || a.Kind != prolink.KindKeepAlive {
		o.emit(desc, route, payload)
		return
	}
	k := peerKey{a.IP, a.DeviceNumber}
	p := o.peers[k]
	if p == nil {
		p = &Peer{}
		o.peers[k] = p
		o.log.Info("device joined", "name", a.Name, "dev", a.DeviceNumber, "type", a.DeviceType.String(), "ip", a.IP.String(), "mac", prolink.MACString(a.MAC))
	}
	p.Name, p.Device, p.Type, p.IP, p.MAC, p.Class, p.Tail, p.LastSeen = a.Name, a.DeviceNumber, a.DeviceType, a.IP, a.MAC, a.Class, a.Tail, t
	// Keep-alives are steady; only log when anything but the peer count changes.
	summary := fmt.Sprintf("%s|%d|%s|%x|%x", a.Name, a.Class, a.DeviceType, a.MAC, a.Tail)
	o.dedupe(route+"/ka", summary, desc, route, payload)
}

func (o *Observer) cdjStatus(t time.Time, src netip.AddrPort, route string, payload []byte) {
	s, err := prolink.DecodeStatus(payload)
	if err != nil || s.CDJ == nil {
		return
	}
	c := s.CDJ
	for _, p := range o.peers {
		if p.Device == s.Device && p.IP == src.Addr() {
			p.Status = c.String()
		}
	}
	// Interesting fields only: play state, loaded track and its source.
	summary := fmt.Sprintf("%d|%d|%d|%d|%d", c.PlayState, c.TrackID, c.SrcPlayer, c.SrcSlot, c.TrackType)
	o.dedupe(fmt.Sprintf("%s/%d/status", src.Addr(), s.Device), summary, s.String(), route, payload)
}

// Expire removes peers silent for PeerTimeout (relative to now) and logs them.
func (o *Observer) Expire(now time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	for k, p := range o.peers {
		if now.Sub(p.LastSeen) > PeerTimeout {
			delete(o.peers, k)
			o.log.Info("device left (no keep-alive)", "name", p.Name, "dev", p.Device, "ip", p.IP.String(), "silent", now.Sub(p.LastSeen))
		}
	}
}

// Peers returns a snapshot of known devices sorted by device number.
func (o *Observer) Peers() []Peer {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]Peer, 0, len(o.peers))
	for _, p := range o.peers {
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Device < out[b].Device })
	return out
}

// Summary logs the peer table and packet counts.
func (o *Observer) Summary() {
	peers := o.Peers()
	o.mu.Lock()
	keys := make([]string, 0, len(o.counts))
	for k := range o.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, o.counts[k]))
	}
	o.mu.Unlock()
	o.log.Info(fmt.Sprintf("%d device(s) on the link", len(peers)))
	for _, p := range peers {
		line := fmt.Sprintf("  #%-2d %-20s %-9s %-15s %s", p.Device, p.Name, p.Type, p.IP, prolink.MACString(p.MAC))
		if p.Status != "" {
			line += "  " + p.Status
		}
		o.log.Info(line)
	}
	if len(parts) > 0 {
		o.log.Info("packet counts: " + strings.Join(parts, " "))
	}
}
