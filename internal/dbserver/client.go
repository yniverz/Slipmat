// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

// Client queries another device's dbserver (for example a CDJ-3000 for the
// analysis it made of a track itself). [DS]
type Client struct {
	conn   net.Conn
	rd     *Reader
	dev    uint8
	txid   uint32
	Server uint32 // device number the server reported in setup
}

// Dial discovers the dbserver port of the device at ip (TCP 12523),
// connects, greets and identifies as device dev.
func Dial(ctx context.Context, ip netip.Addr, dev uint8) (*Client, error) {
	var d net.Dialer
	dctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	disc, err := d.DialContext(dctx, "tcp4", netip.AddrPortFrom(ip, PortDiscovery).String())
	if err != nil {
		return nil, fmt.Errorf("dbclient: discovery: %w", err)
	}
	disc.SetDeadline(time.Now().Add(3 * time.Second))
	_, err = disc.Write(discoveryQuery)
	var port [2]byte
	if err == nil {
		_, err = io.ReadFull(disc, port[:])
	}
	disc.Close()
	if err != nil {
		return nil, fmt.Errorf("dbclient: discovery: %w", err)
	}
	conn, err := d.DialContext(dctx, "tcp4", netip.AddrPortFrom(ip, binary.BigEndian.Uint16(port[:])).String())
	if err != nil {
		return nil, fmt.Errorf("dbclient: connect: %w", err)
	}
	c := &Client{conn: conn, rd: NewReader(conn), dev: dev}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte{fieldNum4, 0, 0, 0, 1}); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := c.rd.ReadGreeting(); err != nil {
		conn.Close()
		return nil, fmt.Errorf("dbclient: greeting: %w", err)
	}
	// Setup as the CDJ-3000 sends it: [our device, 0x14]. [RB7]
	resp, err := c.roundTrip(&Message{TxID: SetupTxID, Type: ReqSetup, Args: []Arg{Num(uint32(dev)), Num(0x14)}})
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("dbclient: setup: %w", err)
	}
	c.Server = resp.NumArg(0)
	if resp.Type == RespSuccess {
		c.Server = resp.NumArg(1)
	}
	return c, nil
}

// Close ends the session.
func (c *Client) Close() error {
	c.conn.SetDeadline(time.Now().Add(time.Second))
	c.conn.Write((&Message{TxID: c.next(), Type: ReqTeardown}).Encode())
	return c.conn.Close()
}

func (c *Client) next() uint32 { c.txid++; return c.txid }

// DMST builds the first request argument: our device, menu location, slot
// and track type.
func (c *Client) DMST(menu, slot, typ uint8) uint32 {
	return uint32(c.dev)<<24 | uint32(menu)<<16 | uint32(slot)<<8 | uint32(typ)
}

func (c *Client) roundTrip(m *Message) (*Message, error) {
	c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(m.Encode()); err != nil {
		return nil, err
	}
	for {
		r, err := c.rd.ReadMessage()
		if err != nil {
			return nil, err
		}
		if r.TxID == m.TxID {
			return r, nil
		}
	}
}

// Request sends a single-reply request (data requests such as 0x2204).
func (c *Client) Request(typ uint16, args ...Arg) (*Message, error) {
	return c.roundTrip(&Message{TxID: c.next(), Type: typ, Args: args})
}

// Menu sends a menu request and renders all of its items.
func (c *Client) Menu(typ uint16, args ...Arg) ([]*Message, error) {
	r, err := c.Request(typ, args...)
	if err != nil {
		return nil, err
	}
	if r.Type != RespSuccess {
		return []*Message{r}, nil
	}
	n := r.NumArg(1)
	if n == 0 || n == 0xffffffff {
		return []*Message{r}, nil
	}
	dmst := args[0].Num
	render := &Message{TxID: c.next(), Type: ReqRenderMenu, Args: []Arg{Num(dmst), Num(0), Num(n), Num(0), Num(n), Num(0xc), Num(1), Num(0)}}
	c.conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(render.Encode()); err != nil {
		return nil, err
	}
	var out []*Message
	for {
		m, err := c.rd.ReadMessage()
		if err != nil {
			return out, err
		}
		if m.TxID != render.TxID {
			continue
		}
		out = append(out, m)
		if m.Type == RespMenuFooter {
			return out, nil
		}
		if len(out) > int(n)+2 {
			return out, errors.New("dbclient: menu longer than announced")
		}
	}
}
