// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"

	"github.com/yniverz/slipmat/internal/dbserver"
	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/pcap"
)

// tcpStream reassembles one direction of a TCP connection well enough for
// protocol analysis: in-order data is appended, retransmitted bytes are
// skipped, and a gap (missed packets) resets the stream.
type tcpStream struct {
	next    uint32
	started bool
	buf     []byte
	greeted bool // dbserver: the initial number field has been seen
	broken  bool
}

type flowKey struct{ src, dst netip.AddrPort }

// add appends the in-order part of p's payload.
func (s *tcpStream) add(p *pcap.Packet) (gap bool) {
	if p.Flags&pcap.TCPSyn != 0 {
		*s = tcpStream{next: p.Seq + 1, started: true}
		return false
	}
	data := p.Payload
	if !s.started {
		s.next, s.started = p.Seq, true
	}
	if len(data) == 0 {
		return false
	}
	switch diff := int32(p.Seq - s.next); {
	case diff > 0: // missing data
		s.buf, s.broken, s.next = nil, true, p.Seq+uint32(len(data))
		return true
	case diff < 0: // retransmission or overlap
		if int(-diff) >= len(data) {
			return false
		}
		data = data[-diff:]
	}
	s.buf = append(s.buf, data...)
	s.next += uint32(len(data))
	return false
}

// dbFlow decodes dbserver messages from reassembled TCP data.
func (d *CaptureDecoder) dbFlow(log *slog.Logger, p *pcap.Packet, request bool) {
	if d.streams == nil {
		d.streams = map[flowKey]*tcpStream{}
	}
	k := flowKey{p.Src, p.Dst}
	s := d.streams[k]
	if s == nil {
		s = &tcpStream{}
		d.streams[k] = s
	}
	dir := "db <- "
	if request {
		dir = "db -> "
	}
	route := fmt.Sprintf("%s -> %s", p.Src, p.Dst)
	if s.add(p) {
		log.Warn(dir+"capture is missing TCP data; skipping until the next message boundary is unknowable", "route", route)
	}
	if s.broken {
		return
	}
	for len(s.buf) > 0 {
		if !s.greeted {
			v, n, err := dbserver.ParseNum(s.buf)
			if err != nil {
				if errors.Is(err, io.ErrUnexpectedEOF) {
					return
				}
				log.Warn(dir+"bad greeting", "route", route, "err", err, "hex", logx.Dump(s.buf))
				s.broken = true
				return
			}
			s.greeted = true
			s.buf = s.buf[n:]
			log.Info(fmt.Sprintf("%sgreeting %d", dir, v), "route", route)
			continue
		}
		m, n, err := dbserver.Parse(s.buf)
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
				return // wait for more data
			}
			log.Warn(dir+"undecodable dbserver data", "route", route, "err", err, "hex", logx.Dump(s.buf[:min(len(s.buf), 256)]))
			s.broken = true
			return
		}
		raw := s.buf[:n]
		s.buf = s.buf[n:]
		if logx.TraceEnabled() {
			log.Info(dir+m.String(), "route", route, "hex", logx.Dump(raw))
		} else {
			log.Info(dir+m.String(), "route", route)
		}
	}
}
