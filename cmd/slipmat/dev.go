// SPDX-License-Identifier: GPL-3.0-or-later

package main

// Developer commands for poking at players: remote-load a track and send
// raw dbserver queries. They exist to find out how players behave; normal
// use doesn't need them.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yniverz/slipmat/internal/dbserver"
	"github.com/yniverz/slipmat/internal/library"
	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/netif"
	"github.com/yniverz/slipmat/internal/prolink"
	"github.com/yniverz/slipmat/internal/waveform"
)

// cmdLoad tells a player to load a track from a running `slipmat serve`.
func cmdLoad(args []string) error {
	fs := flag.NewFlagSet("load", flag.ContinueOnError)
	var c common
	c.register(fs)
	ip := fs.String("ip", "", "IP of the player to load on (required)")
	player := fs.Uint("player", 1, "player number to load on")
	track := fs.String("track", "", "track id (decimal or 0x hex) as served by slipmat (required)")
	dev := fs.Uint("device-number", 17, "our device number (as used by slipmat serve)")
	if err := parse(fs, args); err != nil {
		return err
	}
	logx.Stderr(c.verbose)
	if *ip == "" || *track == "" {
		return errors.New("usage: slipmat load -ip PLAYER_IP -player N -track ID")
	}
	addr, err := netip.ParseAddr(*ip)
	if err != nil {
		return err
	}
	id, err := strconv.ParseUint(*track, 0, 32)
	if err != nil {
		return err
	}
	ctx, cancel := signalContext()
	defer cancel()
	ifc, err := c.chooseInterface(ctx)
	if err != nil {
		return err
	}
	conn, err := netif.ListenUDP(ctx, ifc, 0)
	if err != nil {
		return err
	}
	defer conn.Close()
	pkt := prolink.EncodeLoadTrack("Slipmat", uint8(*dev), uint8(*dev), prolink.SlotRekordbox, 1, uint32(id), uint8(*player))
	if _, err := conn.WriteToUDPAddrPort(pkt, netip.AddrPortFrom(addr, prolink.PortStatus)); err != nil {
		return err
	}
	fmt.Printf("sent load of track %#x to player %d at %s (watch the serve log for the player's 0x1a ack)\n", id, *player, addr)
	return nil
}

// cmdQuery sends dbserver requests to a player and prints the replies.
//
//	slipmat query -ip 192.168.1.101 -track 0x52024286 -slot 4 -type 1 grid preview detail PWV4 PWV5 PWV6 PWV7 PQT2 PSSI meta
func cmdQuery(args []string) error {
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	ip := fs.String("ip", "", "player IP (required)")
	track := fs.String("track", "", "track id (decimal or 0x hex)")
	slot := fs.Uint("slot", uint(prolink.SlotRekordbox), "slot (1 cd, 2 sd, 3 usb, 4 rekordbox)")
	typ := fs.Uint("type", 1, "track type (1 rekordbox, 2 unanalysed, 5 cd)")
	dev := fs.Uint("device-number", 17, "device number to identify as")
	verbose := 0
	fs.Func("v", "hex dumps", func(string) error { verbose += 2; return nil })
	if err := parse(fs, args); err != nil {
		return err
	}
	logx.Stderr(verbose)
	addr, err := netip.ParseAddr(*ip)
	if err != nil {
		return errors.New("-ip is required")
	}
	id, _ := strconv.ParseUint(*track, 0, 32)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cl, err := dbserver.Dial(ctx, addr, uint8(*dev))
	if err != nil {
		return err
	}
	defer cl.Close()
	fmt.Printf("connected; server says it is device %d\n", cl.Server)
	data := dbserver.Num(cl.DMST(8, uint8(*slot), uint8(*typ)))
	popup := dbserver.Num(cl.DMST(2, uint8(*slot), uint8(*typ)))
	tid := dbserver.Num(uint32(id))
	le := func(s string) uint32 {
		var v uint32
		for i := len(s) - 1; i >= 0; i-- {
			v = v<<8 | uint32(s[i])
		}
		return v
	}
	show := func(name string, m *dbserver.Message, err error) {
		if err != nil {
			fmt.Printf("%-8s error: %v\n", name, err)
			return
		}
		fmt.Printf("%-8s %s\n", name, m)
		if logx.TraceEnabled() {
			fmt.Println(logx.Dump(m.Encode()))
		}
	}
	for _, what := range fs.Args() {
		switch w := strings.ToUpper(what); w {
		case "GRID":
			m, err := cl.Request(dbserver.ReqBeatGrid, data, tid)
			show(what, m, err)
		case "PREVIEW":
			m, err := cl.Request(dbserver.ReqWavePreview, data, dbserver.Num(0), tid, dbserver.Num(0), dbserver.Blob(nil))
			show(what, m, err)
		case "DETAIL":
			m, err := cl.Request(dbserver.ReqWaveDetail, data, tid, dbserver.Num(0))
			show(what, m, err)
		case "CUES":
			m, err := cl.Request(dbserver.ReqCuePointsExt, data, tid, dbserver.Num(0))
			show(what, m, err)
		case "PVBR":
			m, err := cl.Request(dbserver.ReqSeekIndex, data, tid)
			show(what, m, err)
		case "PWV4", "PWV5", "PQT2", "PSSI", "PWV3", "PQTZ":
			m, err := cl.Request(dbserver.ReqAnalysisTag, data, tid, dbserver.Num(le(w)), dbserver.Num(le("EXT\x00")))
			show(what, m, err)
		case "PWV6", "PWV7", "PWVC":
			m, err := cl.Request(dbserver.ReqAnalysisTag2EX, data, tid, dbserver.Num(le(w)), dbserver.Num(le("2EX\x00")))
			show(what, m, err)
		case "META":
			items, err := cl.Menu(dbserver.ReqTrackMetadata, popup, tid)
			for _, it := range items {
				show(what, it, nil)
			}
			if err != nil {
				show(what, nil, err)
			}
		default:
			fmt.Printf("unknown query %q\n", what)
		}
	}
	return nil
}

// cmdRender draws a track's waveforms to a PNG: rekordbox's (if any) above
// Slipmat's generated ones, for visual comparison.
func cmdRender(args []string) error {
	fs := flag.NewFlagSet("render", flag.ContinueOnError)
	music := fs.String("music", "", "music folder (default: the last one served)")
	track := fs.String("track", "", "track id (see `slipmat library`) or part of its file name")
	out := fs.String("out", "waveform.png", "output PNG")
	from := fs.Float64("from", 60, "detail window start, seconds")
	secs := fs.Float64("seconds", 8, "detail window length, seconds")
	width := fs.Int("width", 1200, "image width")
	if err := parse(fs, args); err != nil {
		return err
	}
	logx.Stderr(0)
	if *music == "" {
		*music = netif.LoadConfig().Music
	}
	ctx, cancel := signalContext()
	defer cancel()
	lib, err := scanLibrary(ctx, *music)
	if err != nil {
		return err
	}
	var t *library.Track
	if id, err := strconv.ParseUint(*track, 16, 32); err == nil {
		t, _ = lib.Track(uint32(id))
	}
	if t == nil {
		for _, c := range lib.Tracks() {
			if *track != "" && strings.Contains(strings.ToLower(c.RelPath), strings.ToLower(*track)) {
				t = c
				break
			}
		}
	}
	if t == nil {
		return fmt.Errorf("no track matches %q", *track)
	}
	ix := indexAnalysis(lib, nil)
	samples, err := waveform.Decode(ctx, t.Path)
	if err != nil {
		return err
	}
	ours := waveform.Render(waveform.Generate(samples).Analysis(), *width, *from, *secs)
	img := ours
	if a := ix.Load(t.ID); a != nil {
		rb := waveform.Render(a, *width, *from, *secs)
		h := rb.Bounds().Dy()
		img = image.NewRGBA(image.Rect(0, 0, *width, 2*h+6))
		draw.Draw(img, rb.Bounds(), rb, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(0, h, *width, h+6), &image.Uniform{color.RGBA{200, 40, 40, 255}}, image.Point{}, draw.Src)
		draw.Draw(img, image.Rect(0, h+6, *width, 2*h+6), ours, image.Point{}, draw.Src)
		fmt.Println("top: rekordbox, bottom (below the red line): slipmat")
	} else {
		fmt.Println("no rekordbox analysis for this track; showing slipmat only")
	}
	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		return err
	}
	fmt.Printf("%s -> %s\n", t.RelPath, *out)
	return nil
}
