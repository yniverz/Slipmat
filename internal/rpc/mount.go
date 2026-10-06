// SPDX-License-Identifier: GPL-3.0-or-later

package rpc

import (
	"strings"
	"sync"
)

// Portmap answers portmapper v2 calls (RFC 1833). CDJs look for rekordbox's
// portmapper on UDP 50111 and ask it for MOUNT v1 and NFS v2 over UDP. [RB7]
type Portmap struct {
	mu    sync.RWMutex
	ports map[[3]uint32]uint32 // prog, vers, protocol -> port
}

// Protocol numbers used in GETPORT.
const (
	ProtoTCP = 6
	ProtoUDP = 17
)

// Set registers a service port.
func (p *Portmap) Set(prog, vers, proto, port uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ports == nil {
		p.ports = map[[3]uint32]uint32{}
	}
	p.ports[[3]uint32{prog, vers, proto}] = port
}

// Handle returns the reply to a portmap call.
func (p *Portmap) Handle(c *Call) []byte {
	if c.Vers != 2 {
		return ProgMismatch(c.XID, 2, 2)
	}
	switch c.Proc {
	case 0: // NULL
		return AcceptedReply(c.XID, AcceptSuccess, nil)
	case 3: // GETPORT(prog, vers, prot, port) -> port (0 = not registered)
		r := NewReader(c.Args)
		prog, vers, proto := r.Uint32(), r.Uint32(), r.Uint32()
		r.Uint32()
		if r.Err() != nil {
			return AcceptedReply(c.XID, AcceptGarbageArgs, nil)
		}
		p.mu.RLock()
		port := p.ports[[3]uint32{prog, vers, proto}]
		p.mu.RUnlock()
		var w Writer
		w.Uint32(port)
		return AcceptedReply(c.XID, AcceptSuccess, w.B)
	}
	return AcceptedReply(c.XID, AcceptProcUnavail, nil)
}

// MOUNT v1 status codes (subset of NFS v2 stat).
const (
	MountOK     = 0
	MountNoEnt  = 2
	MountAccess = 13
)

// FHSize is the NFS v2 file handle size.
const FHSize = 32

// Export is one entry in the export list.
type Export struct {
	Dir    string
	Groups []string
}

// Mount answers MOUNT v1 calls (RFC 1094 appendix A).
type Mount struct {
	// Exports returns the current export list. An empty list tells CDJs
	// the source is not (or no longer) available. [RB7]
	Exports func() []Export
	// Root is the file handle returned for a successful MNT of an export.
	// rekordbox 7 returns 32 zero bytes. [RB7]
	Root [FHSize]byte
	// Event, if set, is called on MNT/UMNT with the procedure name and path.
	Event func(proc, path string)
}

// Handle returns the reply to a mount call.
func (m *Mount) Handle(c *Call) []byte {
	if c.Vers != 1 {
		return ProgMismatch(c.XID, 1, 1)
	}
	switch c.Proc {
	case 0, 4: // NULL, UMNTALL
		return AcceptedReply(c.XID, AcceptSuccess, nil)
	case 1: // MNT(dirpath) -> fhstatus
		path, ok := m.path(c)
		if !ok {
			return AcceptedReply(c.XID, AcceptGarbageArgs, nil)
		}
		var w Writer
		if !m.exported(path) {
			w.Uint32(MountNoEnt)
		} else {
			w.Uint32(MountOK)
			w.Fixed(m.Root[:])
		}
		if m.Event != nil {
			m.Event("MNT", path)
		}
		return AcceptedReply(c.XID, AcceptSuccess, w.B)
	case 2: // DUMP -> empty mount list
		var w Writer
		w.Uint32(0)
		return AcceptedReply(c.XID, AcceptSuccess, w.B)
	case 3: // UMNT(dirpath) -> void
		if path, ok := m.path(c); ok && m.Event != nil {
			m.Event("UMNT", path)
		}
		return AcceptedReply(c.XID, AcceptSuccess, nil)
	case 5: // EXPORT -> exportlist
		return AcceptedReply(c.XID, AcceptSuccess, encodeExports(m.list()))
	}
	return AcceptedReply(c.XID, AcceptProcUnavail, nil)
}

func (m *Mount) list() []Export {
	if m.Exports == nil {
		return nil
	}
	return m.Exports()
}

// path decodes a dirpath argument. CDJs (and rekordbox) include a trailing
// NUL in the string ("/\x00"). [RB7]
func (m *Mount) path(c *Call) (string, bool) {
	r := NewReader(c.Args)
	p := r.String(1024)
	if r.Err() != nil {
		return "", false
	}
	return strings.TrimRight(p, "\x00"), true
}

func (m *Mount) exported(path string) bool {
	for _, e := range m.list() {
		if e.Dir == path {
			return true
		}
	}
	return false
}

// encodeExports writes the XDR export list. Directory names carry a
// trailing NUL like rekordbox's ("/\x00"); group names do not. [RB7]
func encodeExports(list []Export) []byte {
	var w Writer
	for _, e := range list {
		w.Uint32(1)
		w.String(e.Dir + "\x00")
		for _, g := range e.Groups {
			w.Uint32(1)
			w.String(g)
		}
		w.Uint32(0)
	}
	w.Uint32(0)
	return w.B
}
