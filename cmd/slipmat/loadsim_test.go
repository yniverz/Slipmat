// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"context"
	"log/slog"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/yniverz/slipmat/internal/anlz"
	"github.com/yniverz/slipmat/internal/dbserver"
	"github.com/yniverz/slipmat/internal/library"
	"github.com/yniverz/slipmat/internal/logx"
)

// TestLoadSequenceOffline replays a CDJ-3000's load-time requests (as seen
// from rekordbox 7) against a real dbserver on loopback, for one track with
// rekordbox analysis and one with only generated waveforms. Local only:
// SLIPMAT_LOADSIM_MUSIC=<music folder>.
func TestLoadSequenceOffline(t *testing.T) {
	dir := os.Getenv("SLIPMAT_LOADSIM_MUSIC")
	if dir == "" {
		t.Skip("SLIPMAT_LOADSIM_MUSIC not set")
	}
	logx.Setup(os.Stderr, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lib, err := scanLibrary(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	ix := indexAnalysis(lib, nil)
	waves := startWaveforms(ctx, lib, ix)
	for waves.Pending() > 0 || waves.Busy() > 0 {
		time.Sleep(200 * time.Millisecond)
	}
	srv := &dbserver.Server{Lib: func() *library.Library { return lib }, Device: 17, Log: slog.New(slog.DiscardHandler),
		Analysis: func(id uint32) *anlz.Analysis {
			if a := ix.Load(id); a != nil {
				return a
			}
			return waves.Get(id)
		}}
	if err := srv.Listen(netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Skipf("listen: %v", err)
	}
	go srv.Serve(ctx)

	var analysed, generated *library.Track
	for _, tr := range lib.Tracks() {
		if _, ok := ix.Set(tr.ID); ok && analysed == nil {
			analysed = tr
		} else if !ok && generated == nil {
			generated = tr
		}
	}
	cl, err := dbserver.Dial(ctx, netip.MustParseAddr("127.0.0.1"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer cl.Close()
	n := dbserver.Num
	le := func(s string) uint32 { return uint32(s[0]) | uint32(s[1])<<8 | uint32(s[2])<<16 | uint32(s[3])<<24 }
	for _, tr := range []*library.Track{analysed, generated} {
		data := n(0x01080401)
		reqs := []struct {
			name     string
			typ      uint16
			args     []dbserver.Arg
			wantData bool
		}{
			{"beat grid", dbserver.ReqBeatGrid, []dbserver.Arg{data, n(tr.ID)}, tr == analysed},
			{"PWV6", dbserver.ReqAnalysisTag2EX, []dbserver.Arg{data, n(tr.ID), n(le("PWV6")), n(le("2EX\x00"))}, true},
			{"preview", dbserver.ReqWavePreview, []dbserver.Arg{data, n(0), n(tr.ID), n(0), dbserver.Blob(nil)}, true},
			{"PWV7", dbserver.ReqAnalysisTag2EX, []dbserver.Arg{data, n(tr.ID), n(le("PWV7")), n(le("2EX\x00"))}, true},
			{"PWV5", dbserver.ReqAnalysisTag, []dbserver.Arg{data, n(tr.ID), n(le("PWV5")), n(le("EXT\x00"))}, true},
			{"detail", dbserver.ReqWaveDetail, []dbserver.Arg{data, n(tr.ID), n(0)}, true},
			{"PWV4", dbserver.ReqAnalysisTag, []dbserver.Arg{data, n(tr.ID), n(le("PWV4")), n(le("EXT\x00"))}, true},
		}
		for _, r := range reqs {
			m, err := cl.Request(r.typ, r.args...)
			if err != nil {
				t.Fatalf("%s %s: %v", tr.Title, r.name, err)
			}
			has := len(m.Args) > 3 && m.Args[3].Kind == dbserver.KindBlob && len(m.Args[3].Blob) > 0
			if has != r.wantData {
				t.Errorf("%s %s: data=%v, want %v (%v)", tr.Title, r.name, has, r.wantData, m)
			}
			t.Logf("%-45.45s %-9s %s", tr.Title, r.name, m)
		}
	}
}
