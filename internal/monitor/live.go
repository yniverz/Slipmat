// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/yniverz/slipmat/internal/netif"
	"github.com/yniverz/slipmat/internal/prolink"
)

// Listen opens the passive monitor sockets (50000, 50001, 50002) on ifc.
// It sends nothing, so players only reveal what they broadcast; unicast
// traffic between other devices is invisible without a capture.
func Listen(ctx context.Context, ifc netif.Interface) ([]*net.UDPConn, error) {
	var conns []*net.UDPConn
	for _, port := range []int{prolink.PortAnnounce, prolink.PortBeat, prolink.PortStatus} {
		c, err := netif.ListenUDP(ctx, ifc, port)
		if err != nil {
			for _, c := range conns {
				c.Close()
			}
			return nil, err
		}
		conns = append(conns, c)
	}
	return conns, nil
}

// Run feeds packets from conns into obs until ctx is cancelled, expiring
// silent peers and logging a summary every summaryEvery (0 = never).
func Run(ctx context.Context, conns []*net.UDPConn, obs *Observer, log *slog.Logger, summaryEvery time.Duration) {
	type rx struct {
		t        time.Time
		src, dst netip.AddrPort
		b        []byte
	}
	ch := make(chan rx, 256)
	for _, c := range conns {
		go func(c *net.UDPConn) {
			local := c.LocalAddr().(*net.UDPAddr)
			buf := make([]byte, 65536)
			for {
				n, from, err := c.ReadFromUDPAddrPort(buf)
				if err != nil {
					if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
						return
					}
					log.Warn("socket read failed", "port", local.Port, "err", err)
					time.Sleep(100 * time.Millisecond)
					continue
				}
				dst := netip.AddrPortFrom(netip.IPv4Unspecified(), uint16(local.Port))
				select {
				case ch <- rx{time.Now(), netip.AddrPortFrom(from.Addr().Unmap(), from.Port()), dst, append([]byte(nil), buf[:n]...)}:
				case <-ctx.Done():
					return
				}
			}
		}(c)
	}
	go func() {
		<-ctx.Done()
		for _, c := range conns {
			c.Close()
		}
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	var summary <-chan time.Time
	if summaryEvery > 0 {
		st := time.NewTicker(summaryEvery)
		defer st.Stop()
		summary = st.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case p := <-ch:
			safe(log, func() { obs.Packet(p.t, p.src, p.dst, p.b) })
		case now := <-tick.C:
			obs.Expire(now)
		case <-summary:
			obs.Summary()
		}
	}
}

// safe runs f, converting a panic into an error log so one bad packet can't
// take the process down.
func safe(log *slog.Logger, f func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("BUG: panic while handling packet", "panic", fmt.Sprint(r))
		}
	}()
	f()
}
