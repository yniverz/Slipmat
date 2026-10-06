// SPDX-License-Identifier: GPL-3.0-or-later

package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"

	"github.com/yniverz/slipmat/internal/logx"
)

// Handler answers calls for one program.
type Handler interface {
	Handle(*Call) []byte
}

// Server serves RPC calls on one UDP socket, dispatching by program.
type Server struct {
	Conn     *net.UDPConn
	Programs map[uint32]Handler
	Log      *slog.Logger
	// Workers > 1 handles calls concurrently (NFS: players read from
	// several sockets in parallel). [RB7]
	Workers int
}

// Port returns the local UDP port.
func (s *Server) Port() int { return s.Conn.LocalAddr().(*net.UDPAddr).Port }

// Serve answers calls until ctx is cancelled. Malformed packets and replies
// are logged and dropped; a panic in a handler is logged, not fatal.
func (s *Server) Serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.Conn.Close()
	}()
	sem := make(chan struct{}, max(s.Workers, 1))
	buf := make([]byte, 65536)
	for {
		n, from, err := s.Conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.Log.Warn("rpc read failed", "port", s.Port(), "err", err)
			continue
		}
		pkt := append([]byte(nil), buf[:n]...)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem }()
			if reply := s.handle(pkt, from); reply != nil {
				if _, err := s.Conn.WriteToUDPAddrPort(reply, from); err != nil {
					s.Log.Warn("rpc reply failed", "to", from.String(), "len", len(reply), "err", err)
				}
			}
		}()
	}
}

func (s *Server) handle(pkt []byte, from netip.AddrPort) (reply []byte) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("BUG: panic in rpc handler; call dropped", "panic", fmt.Sprint(r), "from", from.String(), "hex", logx.Dump(pkt))
			reply = nil
		}
	}()
	c, err := ParseCall(pkt)
	if err != nil {
		s.Log.Warn("ignoring malformed rpc packet", "from", from.String(), "port", s.Port(), "err", err, "hex", logx.Dump(pkt))
		return nil
	}
	h := s.Programs[c.Prog]
	if h == nil {
		s.Log.Warn("rpc call for a program we don't serve", "call", c.String(), "from", from.String())
		return AcceptedReply(c.XID, AcceptProgUnavail, nil)
	}
	reply = h.Handle(c)
	if logx.TraceEnabled() {
		logx.Trace(s.Log, "rpc "+c.String(), "from", from.String(), "hex", logx.Dump(pkt))
		logx.Trace(s.Log, "rpc reply", "to", from.String(), "hex", logx.Dump(reply))
	} else {
		s.Log.Debug("rpc "+c.String(), "from", from.String(), "reply_len", len(reply))
	}
	return reply
}
