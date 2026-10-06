// SPDX-License-Identifier: GPL-3.0-or-later

// Package device runs the network presence of a virtual rekordbox: the
// device-number claim and keep-alives on UDP 50000 and the link handshake
// on UDP 50002.
//
// The sequence is adapted from Vynull's device/device.go (GPL-3.0), which
// was derived from rekordbox captures, and is cross-checked against
// beat-link's VirtualRekordbox. Every step logs what it sends so it can be
// compared with a capture of real rekordbox.
package device

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/monitor"
	"github.com/yniverz/slipmat/internal/netif"
	"github.com/yniverz/slipmat/internal/prolink"
)

// Timing. [VN] unless noted.
const (
	claimInterval     = 300 * time.Millisecond // [DS] 300 ms between claim packets
	keepAliveInterval = 1500 * time.Millisecond
	burstInterval     = 50 * time.Millisecond // startup claim-2 burst
	maxBurst          = 200                   // ~10 s, then stop
	statusInterval    = 100 * time.Millisecond
	preflightDuration = 3 * time.Second
	secondRBStatus    = 3 * time.Second
)

// rekordboxSlots are the device numbers rekordbox cycles through in its
// second-stage claims. [VN] (verify against a rekordbox 7 capture)
var rekordboxSlots = []uint8{17, 18, 41, 42, 43, 44}

// Config describes the virtual rekordbox.
type Config struct {
	Interface    netif.Interface
	Name         string // device name in packets (max 20 bytes)
	Host         string // computer name shown on the CDJ (0x11 packet)
	DeviceNumber uint8  // 17 by default (rekordbox)
	Media        prolink.MediaInfo
	DevSetting   prolink.DevSetting
	Force        bool // start even if another rekordbox is on the link
}

// Device is a running virtual rekordbox.
type Device struct {
	cfg Config
	id  prolink.Identity
	log *slog.Logger
	obs *monitor.Observer

	announce, beat, status packetConn

	mu      sync.Mutex
	linked  map[netip.Addr]int // CDJ IP -> number of 0x46 link pings seen
	peers   map[uint8]netip.Addr
	linkedC chan struct{} // closed on the first 0x46
	once    sync.Once
}

// New validates cfg and returns a Device.
func New(cfg Config, log *slog.Logger) (*Device, error) {
	if !cfg.Interface.Prefix.Addr().Is4() {
		return nil, errors.New("device: interface has no IPv4 address")
	}
	if cfg.DeviceNumber == 0 {
		cfg.DeviceNumber = 17
	}
	if cfg.Name == "" {
		cfg.Name = "Slipmat"
	}
	if cfg.DevSetting == (prolink.DevSetting{}) {
		cfg.DevSetting = prolink.DefaultDevSetting
	}
	d := &Device{
		cfg: cfg,
		id: prolink.Identity{
			Name:         cfg.Name,
			DeviceNumber: cfg.DeviceNumber,
			MAC:          cfg.Interface.MAC,
			IP:           cfg.Interface.Prefix.Addr(),
		},
		log:     log,
		linked:  map[netip.Addr]int{},
		peers:   map[uint8]netip.Addr{},
		linkedC: make(chan struct{}),
	}
	d.obs = monitor.NewObserver(logx.Component("link"))
	d.obs.Self = d.id.IP
	return d, nil
}

// Observer exposes the traffic observer (peer table).
func (d *Device) Observer() *monitor.Observer { return d.obs }

// Run brings the device up and blocks until ctx is cancelled.
func (d *Device) Run(ctx context.Context) error {
	ifc := d.cfg.Interface
	var conns []*net.UDPConn
	for _, port := range []int{prolink.PortAnnounce, prolink.PortBeat, prolink.PortStatus} {
		c, err := netif.ListenUDP(ctx, ifc, port)
		if err != nil {
			return err
		}
		defer c.Close()
		conns = append(conns, c)
	}
	d.announce, d.beat, d.status = conns[0], conns[1], conns[2]

	rx := make(chan inbound, 256)
	for _, c := range conns {
		go d.reader(ctx, c, rx)
	}

	d.log.Info("listening before claiming", "for", preflightDuration, "interface", ifc.String())
	if err := d.preflight(ctx, rx); err != nil {
		return err
	}

	d.log.Info("claiming device number", "dev", d.id.DeviceNumber, "name", d.id.Name, "ip", d.id.IP.String(), "mac", prolink.MACString(d.id.MAC))
	claimDone := make(chan error, 1)
	go func() { claimDone <- d.claim(ctx) }()

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	burst := time.NewTicker(burstInterval)
	defer burst.Stop()
	statusTick := time.NewTicker(statusInterval)
	defer statusTick.Stop()
	expire := time.NewTicker(time.Second)
	defer expire.Stop()

	claimed := false
	burstSent := 0
	slotIdx, counter := 0, uint8(1)
	for {
		var burstC <-chan time.Time
		if claimed && burstSent < maxBurst && !d.isLinked() {
			burstC = burst.C
		}
		var statusC <-chan time.Time
		if d.isLinked() {
			statusC = statusTick.C
		}
		select {
		case <-ctx.Done():
			d.log.Info("stopping; players will drop us after their keep-alive timeout")
			return nil
		case err := <-claimDone:
			if err != nil {
				return err
			}
			claimed = true
			d.log.Info("claim sequence finished; sending keep-alives", "dev", d.id.DeviceNumber)
			d.sendKeepAlive()
		case <-keepAlive.C:
			if claimed {
				d.sendKeepAlive()
			}
		case <-burstC:
			// Startup claim-2 burst, cycling slots in pairs until a CDJ
			// links or the cap is reached. [VN]
			d.send(d.announce, d.broadcast(prolink.PortAnnounce), prolink.EncodeRekordboxClaim2(d.id, rekordboxSlots[slotIdx], counter), logx.LevelTrace)
			burstSent++
			if burstSent%2 == 0 {
				if slotIdx++; slotIdx == len(rekordboxSlots) {
					slotIdx, counter = 0, counter+1
				}
			}
			if burstSent == maxBurst {
				d.log.Debug("startup claim burst finished without a CDJ link ping")
			}
		case <-statusC:
			// 0x29 status broadcast in pairs while linked. [VN]
			pkt := prolink.EncodeRBMixerStatus(d.id.Name, d.id.DeviceNumber)
			dst := d.broadcast(prolink.PortStatus)
			d.send(d.status, dst, pkt, logx.LevelTrace)
			d.send(d.status, dst, pkt, logx.LevelTrace)
		case in := <-rx:
			d.handle(in)
		case now := <-expire.C:
			d.obs.Expire(now)
		}
	}
}

// packetConn is the subset of *net.UDPConn the device uses (fakeable in tests).
type packetConn interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	LocalAddr() net.Addr
}

type inbound struct {
	t    time.Time
	port int
	src  netip.AddrPort
	b    []byte
}

func (d *Device) reader(ctx context.Context, c *net.UDPConn, out chan<- inbound) {
	port := c.LocalAddr().(*net.UDPAddr).Port
	buf := make([]byte, 65536)
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			d.log.Warn("socket read failed", "port", port, "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		src := netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		if src.Addr() == d.id.IP {
			continue // our own broadcast looped back
		}
		select {
		case out <- inbound{time.Now(), port, src, append([]byte(nil), buf[:n]...)}:
		case <-ctx.Done():
			return
		}
	}
}

// preflight listens for a while and refuses to start if another rekordbox
// (type 3 keep-alive from a different host) is already on the link.
func (d *Device) preflight(ctx context.Context, rx <-chan inbound) error {
	deadline := time.After(preflightDuration)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			peers := d.obs.Peers()
			d.log.Info(fmt.Sprintf("found %d device(s) on the link", len(peers)))
			for _, p := range peers {
				if p.Type == prolink.DeviceRekordbox && p.IP != d.id.IP {
					msg := fmt.Sprintf("another rekordbox source is on the link: %q (#%d at %s). Two rekordbox sources make CDJs fail to load tracks", p.Name, p.Device, p.IP)
					if !d.cfg.Force {
						return errors.New(msg + "; quit it or pass --force")
					}
					d.log.Warn(msg + "; continuing because of --force")
				}
				if p.Device == d.id.DeviceNumber && p.IP != d.id.IP {
					return fmt.Errorf("device number %d is already used by %q at %s; pick another with --device-number", p.Device, p.Name, p.IP)
				}
			}
			if len(peers) == 0 {
				d.log.Warn("no players seen yet; continuing anyway (they will find us when they come up)")
			}
			return nil
		case in := <-rx:
			d.observe(in)
		}
	}
}

// claim sends the rekordbox claim sequence: claim-1 ×3 (pairs), then
// claim-2 for each rekordbox slot ×6 (pairs), 300 ms apart. [VN]
func (d *Device) claim(ctx context.Context) error {
	dst := d.broadcast(prolink.PortAnnounce)
	for n := uint8(1); n <= 3; n++ {
		pkt := prolink.EncodeRekordboxClaim1(d.id, n)
		d.send(d.announce, dst, pkt, slogDebug)
		d.send(d.announce, dst, pkt, logx.LevelTrace)
		if err := sleep(ctx, claimInterval); err != nil {
			return nil
		}
	}
	for n := uint8(1); n <= 6; n++ {
		for _, slot := range rekordboxSlots {
			pkt := prolink.EncodeRekordboxClaim2(d.id, slot, n)
			d.send(d.announce, dst, pkt, slogDebug)
			d.send(d.announce, dst, pkt, logx.LevelTrace)
		}
		if err := sleep(ctx, claimInterval); err != nil {
			return nil
		}
	}
	return nil
}

const slogDebug = slog.LevelDebug

func (d *Device) sendKeepAlive() {
	peers := uint8(len(d.obs.Peers()) + 1)
	d.send(d.announce, d.broadcast(prolink.PortAnnounce), prolink.EncodeRekordboxKeepAlive(d.id, peers), logx.LevelTrace)
}

// observe passes a packet to the traffic observer for logging.
func (d *Device) observe(in inbound) {
	dst := netip.AddrPortFrom(d.id.IP, uint16(in.port))
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("BUG: panic while logging packet", "panic", fmt.Sprint(r))
		}
	}()
	d.obs.Packet(in.t, in.src, dst, in.b)
}

// handle logs and, where we know the protocol, answers a packet.
func (d *Device) handle(in inbound) {
	d.observe(in)
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("BUG: panic while handling packet; ignored", "panic", fmt.Sprint(r), "src", in.src.String(), "hex", logx.Dump(in.b))
		}
	}()
	switch in.port {
	case prolink.PortAnnounce:
		d.handleAnnounce(in)
	case prolink.PortStatus:
		d.handleStatus(in)
	}
}

func (d *Device) handleAnnounce(in inbound) {
	a, err := prolink.DecodeAnnounce(in.b)
	if err != nil {
		return
	}
	switch a.Kind {
	case prolink.KindKeepAlive:
		d.mu.Lock()
		d.peers[a.DeviceNumber] = a.IP
		d.mu.Unlock()
		if a.DeviceNumber == d.id.DeviceNumber && a.IP != d.id.IP {
			d.log.Error("another device is using our device number", "dev", a.DeviceNumber, "name", a.Name, "ip", a.IP.String())
		}
	case prolink.KindClaim2, prolink.KindClaim3:
		if a.DeviceNumber == d.id.DeviceNumber && a.IP != d.id.IP {
			d.log.Warn("a device is claiming our device number", "dev", a.DeviceNumber, "name", a.Name)
		}
	case prolink.KindConflict:
		d.log.Error("received a device-number conflict packet; another device defends this number", "dev", a.DeviceNumber, "from", in.src.String())
	}
}

func (d *Device) handleStatus(in inbound) {
	s, err := prolink.DecodeStatus(in.b)
	if err != nil {
		return
	}
	reply := netip.AddrPortFrom(in.src.Addr(), prolink.PortStatus) // always to the peer's 50002 [VN]
	switch s.Kind {
	case prolink.KindRBHelloQuery:
		d.log.Info("player asked who we are; sending rekordbox hello", "from", in.src.Addr().String(), "dev", s.Device, "host", d.cfg.Host)
		d.send(d.status, reply, prolink.EncodeRBHello(d.id.Name, d.id.DeviceNumber, d.cfg.Host), slogDebug)

	case prolink.KindMediaQuery:
		q := s.MediaQuery
		if q.Target != d.id.DeviceNumber {
			d.log.Debug("media query for another device; ignoring", "target", q.Target, "slot", q.Slot.String())
			return
		}
		d.log.Info("player asked about our media; sending media response", "from", in.src.Addr().String(), "dev", s.Device, "slot", q.Slot.String(),
			"tracks", d.cfg.Media.Tracks)
		to := reply
		if q.IP.IsValid() && !q.IP.IsUnspecified() {
			to = netip.AddrPortFrom(q.IP, prolink.PortStatus) // [DS] reply to the IP in the query
		}
		pkt := prolink.EncodeMediaResponse(d.id.Name, d.id.DeviceNumber, prolink.SlotRekordbox, d.cfg.Media)
		d.send(d.status, to, pkt, slogDebug)
		d.send(d.status, to, pkt, logx.LevelTrace)

	case prolink.KindLinkPing:
		d.mu.Lock()
		n := d.linked[in.src.Addr()]
		d.linked[in.src.Addr()] = n + 1
		d.mu.Unlock()
		d.once.Do(func() { close(d.linkedC) })
		if n == 0 {
			// First ping: 0x16, and again 3 s later. [VN]
			d.log.Info("player started link handshake", "from", in.src.Addr().String(), "dev", s.Device)
			pkt := prolink.EncodeRBStatus(d.id.Name, d.id.DeviceNumber)
			d.send(d.status, reply, pkt, slogDebug)
			time.AfterFunc(secondRBStatus, func() { d.send(d.status, reply, pkt, slogDebug) })
			return
		}
		// Every later ping is answered with a link activation. [VN]
		if n == 1 {
			d.log.Info("link established; answering link pings with link activation", "from", in.src.Addr().String(), "dev", s.Device)
		}
		d.send(d.status, reply, prolink.EncodeLinkActivate(d.id.Name, d.id.DeviceNumber, prolink.SlotRekordbox, d.cfg.DevSetting), slogDebug)

	case prolink.KindMySettingsReq, prolink.KindMySettingsPut, prolink.KindDevSettingsPut, prolink.KindLoadTrackAck, prolink.KindLoadRejected:
		d.log.Info("not answering (not implemented yet)", "kind", prolink.StatusKindName(s.Kind), "from", in.src.Addr().String())
	}
}

func (d *Device) isLinked() bool {
	select {
	case <-d.linkedC:
		return true
	default:
		return false
	}
}

func (d *Device) broadcast(port int) netip.AddrPort {
	return netip.AddrPortFrom(d.cfg.Interface.Broadcast, uint16(port))
}

// send writes pkt and logs it at lvl (always with a hex dump at trace).
func (d *Device) send(c packetConn, to netip.AddrPort, pkt []byte, lvl slog.Level) {
	_, err := c.WriteToUDPAddrPort(pkt, to)
	if err != nil {
		d.log.Warn("send failed", "to", to.String(), "err", err)
		return
	}
	desc, _ := prolink.Describe(uint16(c.LocalAddr().(*net.UDPAddr).Port), pkt)
	if logx.TraceEnabled() {
		logx.Trace(d.log, "sent "+desc, "to", to.String(), "hex", logx.Dump(pkt))
		return
	}
	d.log.Log(context.Background(), lvl, "sent "+desc, "to", to.String())
}

func sleep(ctx context.Context, dur time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(dur):
		return nil
	}
}
