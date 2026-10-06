// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/yniverz/slipmat/internal/anlz"
	"github.com/yniverz/slipmat/internal/library"
	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/previewcache"
)

// PortDiscovery is where players ask for the dbserver port. [DS]
const PortDiscovery = 12523

// discoveryQuery is what players send to 12523: a 4-byte length (15) and
// "RemoteDBServer\0". [DS][RB7]
var discoveryQuery = append([]byte{0, 0, 0, 0x0f}, "RemoteDBServer\x00"...)

const (
	maxConns    = 32
	idleTimeout = 10 * time.Minute
)

// Server answers dbserver connections.
type Server struct {
	Lib      func() *library.Library
	Device   uint8
	Log      *slog.Logger
	Analysis func(trackID uint32) *anlz.Analysis
	Previews *previewcache.Cache

	disc, db net.Listener
	conns    chan struct{}
	wg       sync.WaitGroup
}

// Listen opens the discovery listener (TCP 12523) and the dbserver
// listener (an ephemeral port, as rekordbox uses) on addr.
func (s *Server) Listen(addr netip.Addr) error {
	var err error
	if s.disc, err = net.Listen("tcp4", netip.AddrPortFrom(addr, PortDiscovery).String()); err != nil {
		return fmt.Errorf("dbserver: listen on TCP %d: %w", PortDiscovery, err)
	}
	if s.db, err = net.Listen("tcp4", netip.AddrPortFrom(addr, 0).String()); err != nil {
		s.disc.Close()
		return fmt.Errorf("dbserver: listen: %w", err)
	}
	s.conns = make(chan struct{}, maxConns)
	return nil
}

// Port is the dbserver port announced through discovery.
func (s *Server) Port() uint16 { return uint16(s.db.Addr().(*net.TCPAddr).Port) }

// Serve accepts connections until ctx is cancelled.
func (s *Server) Serve(ctx context.Context) {
	go func() {
		<-ctx.Done()
		s.disc.Close()
		s.db.Close()
	}()
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.accept(ctx, s.disc, s.discovery) }()
	go func() { defer s.wg.Done(); s.accept(ctx, s.db, s.session) }()
	s.Log.Info("dbserver ready", "discovery", PortDiscovery, "port", s.Port())
	s.wg.Wait()
}

func (s *Server) accept(ctx context.Context, l net.Listener, handle func(context.Context, net.Conn)) {
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			s.Log.Warn("accept failed", "err", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case s.conns <- struct{}{}:
		default:
			s.Log.Warn("too many dbserver connections; refusing", "from", c.RemoteAddr().String())
			c.Close()
			continue
		}
		go func() {
			defer func() { <-s.conns }()
			defer c.Close()
			defer func() {
				if r := recover(); r != nil {
					s.Log.Error("BUG: panic in dbserver connection; closed it", "panic", fmt.Sprint(r), "from", c.RemoteAddr().String())
				}
			}()
			go func() { <-ctx.Done(); c.Close() }()
			handle(ctx, c)
		}()
	}
}

// discovery answers "RemoteDBServer" with our dbserver port.
func (s *Server) discovery(_ context.Context, c net.Conn) {
	c.SetDeadline(time.Now().Add(5 * time.Second))
	buf := make([]byte, len(discoveryQuery))
	if _, err := io.ReadFull(c, buf); err != nil {
		s.Log.Warn("incomplete dbserver discovery query", "from", c.RemoteAddr().String(), "err", err, "hex", logx.Dump(buf))
		return
	}
	if string(buf) != string(discoveryQuery) {
		s.Log.Warn("unexpected dbserver discovery query", "from", c.RemoteAddr().String(), "hex", logx.Dump(buf))
		return
	}
	reply := binary.BigEndian.AppendUint16(nil, s.Port())
	if _, err := c.Write(reply); err != nil {
		s.Log.Warn("discovery reply failed", "err", err)
		return
	}
	s.Log.Debug("told a player our dbserver port", "from", c.RemoteAddr().String(), "port", s.Port())
	// Let the player close first; rekordbox keeps the socket open briefly.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, c)
}

// session runs one dbserver connection: greeting, then requests answered
// strictly in order (players pipeline them).
func (s *Server) session(_ context.Context, c net.Conn) {
	from := c.RemoteAddr().String()
	log := s.Log.With("peer", from)
	rd := NewReader(c)
	w := bufio.NewWriterSize(c, 64<<10)
	c.SetDeadline(time.Now().Add(idleTimeout))
	g, err := rd.ReadGreeting()
	if err != nil {
		log.Warn("dbserver connection without greeting", "err", err)
		return
	}
	if g != 1 {
		log.Warn("unexpected dbserver greeting", "value", g)
	}
	w.Write([]byte{fieldNum4, 0, 0, 0, 1})
	if err := w.Flush(); err != nil {
		return
	}
	log.Info("player connected to dbserver")
	sess := NewSession(s.Lib, s.Device, log)
	sess.Analysis = s.Analysis
	sess.Previews = s.Previews
	for {
		c.SetDeadline(time.Now().Add(idleTimeout))
		m, err := rd.ReadMessage()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				log.Info("player disconnected from dbserver")
			} else {
				log.Warn("dbserver connection ended", "err", err)
			}
			return
		}
		if logx.TraceEnabled() {
			logx.Trace(log, "db request "+m.String(), "hex", logx.Dump(m.Encode()))
		} else {
			log.Debug("db request " + m.String())
		}
		resp := s.safeHandle(sess, m, log)
		for _, r := range resp {
			b := r.Encode()
			if logx.TraceEnabled() {
				logx.Trace(log, "db reply "+r.String(), "hex", logx.Dump(b))
			}
			w.Write(b)
		}
		if err := w.Flush(); err != nil {
			log.Warn("dbserver write failed", "err", err)
			return
		}
		if sess.Closed {
			log.Info("player closed its dbserver session")
			return
		}
	}
}

func (s *Server) safeHandle(sess *Session, m *Message, log *slog.Logger) (resp []*Message) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("BUG: panic handling dbserver request; sent an empty result", "panic", fmt.Sprint(r), "msg", m.String())
			resp = []*Message{success(m, 0)}
		}
	}()
	return sess.Handle(m)
}
