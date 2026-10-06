// SPDX-License-Identifier: GPL-3.0-or-later

// Package device runs the network presence of a virtual rekordbox: the
// device-number claim and keep-alives on UDP 50000, the link handshake on
// UDP 50002, and the portmapper/MOUNT services the players check before
// they list a rekordbox source.
//
// The sequence and timing follow a capture of rekordbox 7 serving two
// CDJ-3000s (fw 3.22) [RB7]:
//
//	claim-1 ×3, claim-2 ×6 rounds over devices {17,18,41,42,43,44}, 100 ms apart
//	keep-alive every 2 s, 0x29 status broadcast every ~100 ms
//	CDJ 0x10  -> 0x11 (host name), then an unprompted 0x16 per CDJ
//	CDJ portmap GETPORT (UDP 50111) -> MOUNT EXPORT -> MNT "/"
//	CDJ 0x05 media query -> 0x06; CDJ 0x46 link ping -> 0x47
//	quit: 0x16 to each CDJ (they UMNT on an empty export), then a 0x08
//	      "leaving" broadcast with our own number, then silence.
//
// All broadcasts go out from fresh ephemeral source ports, as rekordbox
// does; unicast replies come from port 50002.
package device

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/monitor"
	"github.com/yniverz/slipmat/internal/netif"
	"github.com/yniverz/slipmat/internal/prolink"
	"github.com/yniverz/slipmat/internal/rpc"
)

// Timing. [RB7]
const (
	claimInterval     = 100 * time.Millisecond
	claimRounds       = 6
	keepAliveInterval = 2 * time.Second
	statusInterval    = 100 * time.Millisecond
	helloToStatus     = 200 * time.Millisecond // 0x11 -> unprompted 0x16
	quitToLeave       = time.Second            // 0x16 on quit -> 0x08 leave (rekordbox 7 waited ~9 s)
	preflightDuration = 3 * time.Second

	// PortPortmap is rekordbox's non-standard portmapper port. [VN][RB7]
	PortPortmap = 50111
	// PortNFS is what we advertise for NFS v2 (served from milestone 3).
	PortNFS = 2049
)

// rekordboxSlots are the device numbers rekordbox cycles through in its
// second-stage claims. [VN][RB7]
var rekordboxSlots = []uint8{17, 18, 41, 42, 43, 44}

// Config describes the virtual rekordbox.
type Config struct {
	Interface    netif.Interface
	Name         string // device name in packets (max 20 bytes)
	Host         string // computer name shown on the CDJ (0x11 packet)
	DeviceNumber uint8  // 17 by default (rekordbox)
	Media        prolink.MediaInfo
	Force        bool // start even if another rekordbox is on the link
}

// packetConn is the subset of *net.UDPConn the device writes to (fakeable in tests).
type packetConn interface {
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	LocalAddr() net.Addr
}

// Device is a running virtual rekordbox.
type Device struct {
	cfg Config
	id  prolink.Identity
	log *slog.Logger
	obs *monitor.Observer

	status packetConn                       // port 50002: unicast replies
	bcast  func(port int, pkt []byte) error // ephemeral-port broadcast sender
	after  func(time.Duration, func())      // timer (replaced in tests)

	available atomic.Bool // export list non-empty

	mu      sync.Mutex
	helloed map[netip.Addr]bool // CDJs we answered 0x10 for
	linked  map[netip.Addr]bool // CDJs that sent a 0x46 link ping
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
	d := &Device{
		cfg: cfg,
		id: prolink.Identity{
			Name:         cfg.Name,
			DeviceNumber: cfg.DeviceNumber,
			MAC:          cfg.Interface.MAC,
			IP:           cfg.Interface.Prefix.Addr(),
		},
		log:     log,
		after:   func(d time.Duration, f func()) { time.AfterFunc(d, f) },
		helloed: map[netip.Addr]bool{},
		linked:  map[netip.Addr]bool{},
	}
	d.obs = monitor.NewObserver(logx.Component("link"))
	d.obs.Self = d.id.IP
	return d, nil
}

// Observer exposes the traffic observer (peer table).
func (d *Device) Observer() *monitor.Observer { return d.obs }

// Exports is the MOUNT export list: "/" for our own subnet while available,
// empty otherwise (which makes players drop the source). [RB7]
func (d *Device) Exports() []rpc.Export {
	if !d.available.Load() {
		return nil
	}
	p := d.cfg.Interface.Prefix
	mask := net.CIDRMask(p.Bits(), 32)
	group := fmt.Sprintf("%s/%d.%d.%d.%d", p.Addr(), mask[0], mask[1], mask[2], mask[3])
	return []rpc.Export{{Dir: "/", Groups: []string{group}}}
}

// Run brings the device up and blocks until ctx is cancelled, then performs
// rekordbox's quit sequence.
func (d *Device) Run(ctx context.Context) error {
	ifc := d.cfg.Interface
	// Services must outlive ctx so the quit sequence can still answer the
	// players' EXPORT/UMNT calls.
	svcCtx, stopSvc := context.WithCancel(context.Background())
	defer stopSvc()

	var conns []*net.UDPConn
	for _, port := range []int{prolink.PortAnnounce, prolink.PortBeat, prolink.PortStatus, PortPortmap, 0} {
		c, err := netif.ListenUDP(svcCtx, ifc, port)
		if err != nil {
			return err
		}
		defer c.Close()
		conns = append(conns, c)
	}
	d.status = conns[2]
	d.bcast = func(port int, pkt []byte) error { return d.sendEphemeral(svcCtx, port, pkt) }
	if err := d.startRPC(svcCtx, conns[3], conns[4]); err != nil {
		return err
	}

	rx := make(chan inbound, 256)
	for _, c := range conns[:3] {
		go d.reader(svcCtx, c, rx)
	}

	d.log.Info("listening before claiming", "for", preflightDuration, "interface", ifc.String())
	if err := d.preflight(ctx, rx); err != nil {
		return err
	}

	d.log.Info("claiming device number", "dev", d.id.DeviceNumber, "name", d.id.Name, "ip", d.id.IP.String(), "mac", prolink.MACString(d.id.MAC))
	claimDone := make(chan struct{})
	go func() {
		d.claim(ctx)
		close(claimDone)
	}()

	keepAlive := time.NewTicker(keepAliveInterval)
	defer keepAlive.Stop()
	statusTick := time.NewTicker(statusInterval)
	defer statusTick.Stop()
	expire := time.NewTicker(time.Second)
	defer expire.Stop()

	up := false
	for {
		var statusC, keepAliveC <-chan time.Time
		if up {
			statusC, keepAliveC = statusTick.C, keepAlive.C
		}
		select {
		case <-ctx.Done():
			if up {
				d.quit(rx)
			}
			return nil
		case <-claimDone:
			claimDone = nil
			if ctx.Err() != nil {
				continue // interrupted while claiming; never announced
			}
			up = true
			d.available.Store(true)
			d.log.Info("claimed; announcing ourselves as a rekordbox source", "dev", d.id.DeviceNumber, "host", d.cfg.Host)
			d.sendKeepAlive()
		case <-keepAliveC:
			d.sendKeepAlive()
		case <-statusC:
			d.broadcast(prolink.PortStatus, prolink.EncodeRBMixerStatus(d.id.Name, d.id.DeviceNumber), logx.LevelTrace)
		case in := <-rx:
			d.handle(in)
		case now := <-expire.C:
			d.obs.Expire(now)
		}
	}
}

// startRPC serves the portmapper on pm (UDP 50111) and MOUNT on mnt.
func (d *Device) startRPC(ctx context.Context, pm, mnt *net.UDPConn) error {
	log := logx.Component("rpc")
	mount := &rpc.Mount{Exports: d.Exports, Event: d.mountEvent}
	mountSrv := &rpc.Server{Conn: mnt, Programs: map[uint32]rpc.Handler{rpc.ProgMount: mount}, Log: log}
	portmap := &rpc.Portmap{}
	portmap.Set(rpc.ProgMount, 1, rpc.ProtoUDP, uint32(mountSrv.Port()))
	portmap.Set(rpc.ProgNFS, 2, rpc.ProtoUDP, PortNFS)
	pmSrv := &rpc.Server{Conn: pm, Programs: map[uint32]rpc.Handler{rpc.ProgPortmap: portmap}, Log: log}
	go pmSrv.Serve(ctx)
	go mountSrv.Serve(ctx)
	log.Info("portmapper and mount daemon ready", "portmap", pmSrv.Port(), "mount", mountSrv.Port(), "nfs", PortNFS)
	return nil
}

func (d *Device) mountEvent(proc, path string) {
	switch proc {
	case "MNT":
		d.log.Info("a player mounted our export", "path", path, "available", d.available.Load())
	case "UMNT":
		d.log.Info("a player unmounted our export", "path", path)
	}
}

// quit tells linked players we're going away, gives them a moment to
// unmount, then broadcasts the leave packet. [RB7]
func (d *Device) quit(rx <-chan inbound) {
	d.available.Store(false)
	d.mu.Lock()
	var peers []netip.Addr
	for ip := range d.helloed {
		peers = append(peers, ip)
	}
	d.mu.Unlock()
	d.log.Info("quitting: telling players the source is going away", "players", len(peers))
	for _, ip := range peers {
		d.send(d.status, netip.AddrPortFrom(ip, prolink.PortStatus), prolink.EncodeRBStatus(d.id.Name, d.id.DeviceNumber), slog.LevelDebug)
	}
	deadline := time.After(quitToLeave)
wait:
	for {
		select {
		case in := <-rx:
			d.observe(in)
		case <-deadline:
			break wait
		}
	}
	d.broadcast(prolink.PortAnnounce, prolink.EncodeRekordboxLeave(d.id), slog.LevelInfo)
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

// isRekordbox reports whether a peer is a rekordbox instance.
func isRekordbox(p monitor.Peer) bool {
	return p.Type == prolink.DeviceRekordbox || p.Name == "rekordbox"
}

// preflight listens for a while and refuses to start if another rekordbox
// is on the link or our device number is taken.
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
				if p.IP == d.id.IP {
					continue
				}
				if isRekordbox(p) {
					msg := fmt.Sprintf("another rekordbox source is on the link: %q (#%d at %s); two rekordbox sources make players fail to load tracks", p.Name, p.Device, p.IP)
					if !d.cfg.Force {
						return errors.New(msg + "; quit it or pass --force")
					}
					d.log.Warn(msg + "; continuing because of --force")
				}
				if p.Device == d.id.DeviceNumber {
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

// claim sends claim-1 ×3 then claim-2 for each rekordbox slot ×6 rounds,
// one packet every 100 ms. [RB7]
func (d *Device) claim(ctx context.Context) {
	tick := func() bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(claimInterval):
			return true
		}
	}
	for n := uint8(1); n <= 3; n++ {
		d.broadcast(prolink.PortAnnounce, prolink.EncodeRekordboxClaim1(d.id, n), slog.LevelDebug)
		if !tick() {
			return
		}
	}
	for n := uint8(1); n <= claimRounds; n++ {
		for _, slot := range rekordboxSlots {
			d.broadcast(prolink.PortAnnounce, prolink.EncodeRekordboxClaim2(d.id, slot, n), slog.LevelDebug)
			if !tick() {
				return
			}
		}
	}
}

func (d *Device) sendKeepAlive() {
	peers := 1
	for _, p := range d.obs.Peers() {
		if p.IP != d.id.IP {
			peers++
		}
	}
	d.broadcast(prolink.PortAnnounce, prolink.EncodeRekordboxKeepAlive(d.id, uint8(peers)), logx.LevelTrace)
}

// observe passes a packet to the traffic observer for logging.
func (d *Device) observe(in inbound) {
	defer func() {
		if r := recover(); r != nil {
			d.log.Error("BUG: panic while logging packet", "panic", fmt.Sprint(r))
		}
	}()
	d.obs.Packet(in.t, in.src, netip.AddrPortFrom(d.id.IP, uint16(in.port)), in.b)
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
		if a.DeviceNumber == d.id.DeviceNumber && a.IP != d.id.IP {
			d.log.Error("another device is using our device number", "dev", a.DeviceNumber, "name", a.Name, "ip", a.IP.String())
		}
	case prolink.KindClaim2, prolink.KindClaim3:
		if a.DeviceNumber == d.id.DeviceNumber && a.IP != d.id.IP {
			d.log.Warn("a device is claiming our device number", "dev", a.DeviceNumber, "name", a.Name)
		}
	case prolink.KindConflict:
		if a.IP == in.src.Addr() {
			d.log.Info("a device announced it is leaving", "dev", a.DeviceNumber, "name", a.Name, "ip", a.IP.String())
		} else if a.DeviceNumber == d.id.DeviceNumber {
			d.log.Error("received a device-number conflict for our number", "dev", a.DeviceNumber, "from", in.src.String())
		}
	}
}

func (d *Device) handleStatus(in inbound) {
	s, err := prolink.DecodeStatus(in.b)
	if err != nil {
		return
	}
	from := in.src.Addr()
	reply := netip.AddrPortFrom(from, prolink.PortStatus) // always the peer's 50002 [DS][RB7]
	switch s.Kind {
	case prolink.KindRBHelloQuery:
		d.log.Info("player asked who we are; sending rekordbox hello", "from", from.String(), "dev", s.Device, "host", d.cfg.Host)
		d.send(d.status, reply, prolink.EncodeRBHello(d.id.Name, d.id.DeviceNumber, d.cfg.Host), slog.LevelDebug)
		d.mu.Lock()
		first := !d.helloed[from]
		d.helloed[from] = true
		d.mu.Unlock()
		if first {
			// rekordbox follows its hello with an unprompted 0x16, after
			// which the player (re)reads our MOUNT export list. [RB7]
			d.after(helloToStatus, func() {
				d.send(d.status, reply, prolink.EncodeRBStatus(d.id.Name, d.id.DeviceNumber), slog.LevelDebug)
			})
		}

	case prolink.KindMediaQuery:
		q := s.MediaQuery
		if q.Target != d.id.DeviceNumber {
			d.log.Debug("media query for another device; ignoring", "target", q.Target, "slot", q.Slot.String())
			return
		}
		d.log.Info("player asked about our media; sending media response", "from", from.String(), "dev", s.Device, "slot", q.Slot.String(), "tracks", d.cfg.Media.Tracks)
		to := reply
		if q.IP.IsValid() && !q.IP.IsUnspecified() {
			to = netip.AddrPortFrom(q.IP, prolink.PortStatus) // [DS] reply to the IP in the query
		}
		d.send(d.status, to, prolink.EncodeMediaResponse(d.id.Name, d.id.DeviceNumber, prolink.SlotRekordbox, d.cfg.Media), slog.LevelDebug)

	case prolink.KindLinkPing:
		d.mu.Lock()
		first := !d.linked[from]
		d.linked[from] = true
		d.mu.Unlock()
		if first {
			d.log.Info("player linked to us", "from", from.String(), "dev", s.Device)
		}
		d.send(d.status, reply, prolink.EncodeLinkActivate(d.id.Name, d.id.DeviceNumber, prolink.SlotRekordbox), slog.LevelDebug)

	case prolink.KindMySettingsReq, prolink.KindMySettingsPut, prolink.KindDevSettingsPut, prolink.KindLoadTrackAck, prolink.KindLoadRejected:
		d.log.Info("not answering (not implemented yet)", "kind", prolink.StatusKindName(s.Kind), "from", from.String())
	}
}

// broadcast sends pkt to the subnet broadcast address from a fresh
// ephemeral port, as rekordbox does. [RB7]
func (d *Device) broadcast(port int, pkt []byte, lvl slog.Level) {
	to := netip.AddrPortFrom(d.cfg.Interface.Broadcast, uint16(port))
	if err := d.bcast(port, pkt); err != nil {
		d.log.Warn("broadcast failed", "to", to.String(), "err", err)
		return
	}
	d.logSent(to, pkt, lvl)
}

func (d *Device) sendEphemeral(ctx context.Context, port int, pkt []byte) error {
	c, err := netif.ListenUDP(ctx, d.cfg.Interface, 0)
	if err != nil {
		return err
	}
	defer c.Close()
	_, err = c.WriteToUDPAddrPort(pkt, netip.AddrPortFrom(d.cfg.Interface.Broadcast, uint16(port)))
	return err
}

// send writes pkt from conn and logs it at lvl (with a hex dump at trace).
func (d *Device) send(c packetConn, to netip.AddrPort, pkt []byte, lvl slog.Level) {
	if _, err := c.WriteToUDPAddrPort(pkt, to); err != nil {
		d.log.Warn("send failed", "to", to.String(), "err", err)
		return
	}
	d.logSent(to, pkt, lvl)
}

func (d *Device) logSent(to netip.AddrPort, pkt []byte, lvl slog.Level) {
	desc, _ := prolink.Describe(to.Port(), pkt)
	if logx.TraceEnabled() {
		logx.Trace(d.log, "sent "+desc, "to", to.String(), "hex", logx.Dump(pkt))
		return
	}
	d.log.Log(context.Background(), lvl, "sent "+desc, "to", to.String())
}
