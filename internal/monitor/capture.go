// SPDX-License-Identifier: GPL-3.0-or-later

package monitor

import (
	"encoding/binary"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/pcap"
	"github.com/yniverz/slipmat/internal/prolink"
)

// Well-known ports for the server side of Link Export.
const (
	PortDBDiscovery = 12523
	PortPortmap     = 111
	PortPortmapRB   = 50111 // rekordbox's non-standard portmapper
	PortNFS         = 2049
)

// CaptureDecoder walks a capture file and logs every Pro DJ Link packet,
// dbserver TCP segment and ONC-RPC call/reply it finds.
type CaptureDecoder struct {
	Obs *Observer
	Log *slog.Logger
	// Filter limits output to packets involving this host (optional).
	Filter netip.Addr
	// KeepDuplicates disables pktap duplicate suppression (needed to see
	// packets that a device deliberately sends twice).
	KeepDuplicates bool

	dbPorts map[uint16]bool        // dbserver ports announced via 12523
	rpcPort map[uint16]string      // extra RPC ports (mount) learned from portmap
	calls   map[uint32]rpcCallInfo // xid -> call, to label replies
	streams map[flowKey]*tcpStream
	start   time.Time
	expired time.Time
}

type rpcCallInfo struct {
	prog, vers, proc uint32
	getport          uint32 // program asked about in a portmap GETPORT
}

// DecodeFile decodes the capture at path.
func (d *CaptureDecoder) DecodeFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return d.Decode(f)
}

// Decode decodes a capture stream.
func (d *CaptureDecoder) Decode(r io.Reader) error {
	rd, err := pcap.NewReader(r)
	if err != nil {
		return err
	}
	d.dbPorts = map[uint16]bool{}
	d.rpcPort = map[uint16]string{}
	d.calls = map[uint32]rpcCallInfo{}
	dec := pcap.NewDecoder()
	dec.Dedupe = !d.KeepDuplicates
	n := 0
	for {
		f, err := rd.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			d.Log.Warn("capture read stopped early", "err", err, "frames", n)
			break
		}
		n++
		p, err := dec.Decode(f)
		if err != nil {
			d.Log.Debug("skipping undecodable frame", "frame", n, "err", err)
			continue
		}
		if p == nil {
			continue
		}
		if d.Filter.IsValid() && p.Src.Addr() != d.Filter && p.Dst.Addr() != d.Filter {
			continue
		}
		if d.start.IsZero() {
			d.start = p.Time
		}
		safe(d.Log, func() { d.packet(p) })
	}
	d.Obs.log = d.Log
	d.Obs.Summary()
	return nil
}

func isProlinkPort(p uint16) bool {
	return p >= prolink.PortAnnounce && p <= prolink.PortTouchAudio && p != 50003
}

func (d *CaptureDecoder) packet(p *pcap.Packet) {
	rel := p.Time.Sub(d.start)
	log := d.Log.With("t", fmt.Sprintf("%9.3f", rel.Seconds()))
	if p.Time.Sub(d.expired) >= time.Second {
		d.expired = p.Time
		d.Obs.log = log
		d.Obs.Expire(p.Time)
	}
	sp, dp := p.Src.Port(), p.Dst.Port()
	switch p.Proto {
	case pcap.ProtoUDP:
		switch {
		case isProlinkPort(dp) && len(p.Payload) >= prolink.HeaderLen && string(p.Payload[:10]) == string(prolink.Magic[:]):
			d.Obs.log = log // single-threaded here: tag observer output with capture time
			d.Obs.Packet(p.Time, p.Src, p.Dst, p.Payload)
		case isRPCPort(dp) || isRPCPort(sp) || d.rpcPort[dp] != "" || d.rpcPort[sp] != "":
			d.rpc(log, p)
		}
	case pcap.ProtoTCP:
		d.tcp(log, p)
	}
}

func isRPCPort(p uint16) bool {
	return p == PortPortmap || p == PortPortmapRB || p == PortNFS
}

func flagString(f uint8) string {
	var s strings.Builder
	for _, x := range []struct {
		bit uint8
		c   byte
	}{{pcap.TCPSyn, 'S'}, {pcap.TCPFin, 'F'}, {pcap.TCPRst, 'R'}, {pcap.TCPPsh, 'P'}, {pcap.TCPAck, '.'}} {
		if f&x.bit != 0 {
			s.WriteByte(x.c)
		}
	}
	return s.String()
}

func (d *CaptureDecoder) tcp(log *slog.Logger, p *pcap.Packet) {
	sp, dp := p.Src.Port(), p.Dst.Port()
	route := fmt.Sprintf("%s -> %s", p.Src, p.Dst)
	switch {
	case dp == PortDBDiscovery || sp == PortDBDiscovery:
		if len(p.Payload) == 0 {
			return
		}
		if sp == PortDBDiscovery && len(p.Payload) == 2 {
			port := binary.BigEndian.Uint16(p.Payload)
			d.dbPorts[port] = true
			log.Info(fmt.Sprintf("db-discovery reply: dbserver on port %d", port), "route", route)
			return
		}
		what := "db-discovery request"
		if sp == PortDBDiscovery {
			what = "db-discovery reply"
		}
		log.Info(what, "route", route, "len", len(p.Payload), "hex", logx.Dump(p.Payload))
	case d.dbPorts[dp]:
		d.dbFlow(log, p, true)
	case d.dbPorts[sp]:
		d.dbFlow(log, p, false)
	}
}

// ONC RPC program numbers.
const (
	progPortmap = 100000
	progNFS     = 100003
	progMount   = 100005
)

func progName(p uint32) string {
	switch p {
	case progPortmap:
		return "portmap"
	case progNFS:
		return "nfs"
	case progMount:
		return "mount"
	}
	return fmt.Sprintf("prog%d", p)
}

var nfs2Procs = []string{"NULL", "GETATTR", "SETATTR", "ROOT", "LOOKUP", "READLINK", "READ", "WRITECACHE", "WRITE", "CREATE", "REMOVE", "RENAME", "LINK", "SYMLINK", "MKDIR", "RMDIR", "READDIR", "STATFS"}
var mountProcs = []string{"NULL", "MNT", "DUMP", "UMNT", "UMNTALL", "EXPORT"}
var portmapProcs = []string{"NULL", "SET", "UNSET", "GETPORT", "DUMP", "CALLIT"}

func procName(prog, proc uint32) string {
	var names []string
	switch prog {
	case progNFS:
		names = nfs2Procs
	case progMount:
		names = mountProcs
	case progPortmap:
		names = portmapProcs
	}
	if int(proc) < len(names) {
		return names[proc]
	}
	return fmt.Sprintf("proc%d", proc)
}

// rpc logs an ONC RPC call or reply (header level only).
func (d *CaptureDecoder) rpc(log *slog.Logger, p *pcap.Packet) {
	b := p.Payload
	route := fmt.Sprintf("%s -> %s", p.Src, p.Dst)
	if len(b) < 8 {
		log.Warn("short rpc packet", "route", route, "hex", logx.Dump(b))
		return
	}
	xid := binary.BigEndian.Uint32(b[0:4])
	switch binary.BigEndian.Uint32(b[4:8]) {
	case 0: // CALL
		if len(b) < 24 {
			return
		}
		c := rpcCallInfo{prog: binary.BigEndian.Uint32(b[12:16]), vers: binary.BigEndian.Uint32(b[16:20]), proc: binary.BigEndian.Uint32(b[20:24])}
		extra := ""
		if args := rpcArgs(b); c.prog == progPortmap && c.proc == 3 && len(args) >= 4 {
			c.getport = binary.BigEndian.Uint32(args[0:4])
			extra = " for " + progName(c.getport)
		}
		d.calls[xid] = c
		d.rpcLog(log, fmt.Sprintf("rpc call %s v%d %s%s", progName(c.prog), c.vers, procName(c.prog, c.proc), extra), route, xid, b)
	case 1: // REPLY
		c, ok := d.calls[xid]
		label := "rpc reply"
		if ok {
			label = fmt.Sprintf("rpc reply %s %s", progName(c.prog), procName(c.prog, c.proc))
			delete(d.calls, xid)
			// Learn the mount daemon port from portmap GETPORT replies.
			if c.prog == progPortmap && c.proc == 3 && len(b) >= 28 {
				port := binary.BigEndian.Uint32(b[len(b)-4:])
				if port > 0 && port < 65536 {
					d.rpcPort[uint16(port)] = progName(c.getport)
					label += fmt.Sprintf(": %s is on port %d", progName(c.getport), port)
				}
			}
		}
		d.rpcLog(log, label, route, xid, b)
	}
}

func (d *CaptureDecoder) rpcLog(log *slog.Logger, msg, route string, xid uint32, b []byte) {
	if logx.TraceEnabled() {
		log.Info(msg, "route", route, "xid", fmt.Sprintf("%08x", xid), "len", len(b), "hex", logx.Dump(b))
		return
	}
	log.Info(msg, "route", route, "xid", fmt.Sprintf("%08x", xid), "len", len(b))
}

// rpcArgs returns the call arguments after credentials and verifier.
func rpcArgs(b []byte) []byte {
	off := 24
	for i := 0; i < 2; i++ { // cred, verf
		if len(b) < off+8 {
			return nil
		}
		l := int(binary.BigEndian.Uint32(b[off+4 : off+8]))
		off += 8 + (l+3)&^3
		if off > len(b) {
			return nil
		}
	}
	return b[off:]
}
