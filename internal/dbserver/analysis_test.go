// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"encoding/binary"
	"strings"
	"testing"

	"github.com/yniverz/slipmat/internal/anlz"
	"github.com/yniverz/slipmat/internal/previewcache"
)

func sec(tag string, hdr int, fields []byte, body []byte) anlz.Section {
	b := make([]byte, hdr)
	copy(b, tag)
	binary.BigEndian.PutUint32(b[4:], uint32(hdr))
	binary.BigEndian.PutUint32(b[8:], uint32(hdr+len(body)))
	copy(b[12:], fields)
	return anlz.Section{Tag: tag, Header: hdr, Raw: append(b, body...)}
}

func TestAnalysisRepliesMatchRekordboxShapes(t *testing.T) {
	lib := testLib()
	tr := lib.Tracks()[0]
	pqtzHdr := append(make([]byte, 4), 0, 8, 0, 0, 0, 0, 0, 1)
	a := &anlz.Analysis{
		DAT: &anlz.File{Sections: []anlz.Section{
			sec("PQTZ", 24, pqtzHdr, []byte{0, 1, 0x3a, 0x98, 0, 0, 0, 0x78}),
			sec("PWAV", 20, nil, make([]byte, 400)),
			sec("PVBR", 16, nil, make([]byte, 1604)),
		}},
		EXT: &anlz.File{Sections: []anlz.Section{sec("PWV4", 24, nil, make([]byte, 7200)), sec("PWV3", 24, []byte{0, 0, 0, 1, 0, 0, 0, 4, 0, 0x96}, make([]byte, 4))}},
	}
	s := newSession(lib)
	s.Analysis = func(id uint32) *anlz.Analysis {
		if id == tr.ID {
			return a
		}
		return nil
	}
	one := func(m *Message) string { return s.Handle(m)[0].String() }
	// Decoded shapes of rekordbox 7's replies (blob sizes differ). [RB7]
	for _, c := range []struct {
		req  *Message
		want string
	}{
		{req(1, ReqBeatGrid, dmstData, tr.ID), "beat-grid-data(4602) txid=1 args=[0x2204 0x0 0x24 blob[36 bytes] 0x0]"},
		{&Message{TxID: 2, Type: ReqAnalysisTag, Args: []Arg{Num(dmstData), Num(tr.ID), Num(0x34565750), Num(0x00545845)}}, "analysis-tag-data(4f02) txid=2 args=[0x2c04 0x0 0x1c3c blob[7228 bytes] 0x1]"},
		{&Message{TxID: 3, Type: ReqWavePreview, Args: []Arg{Num(dmstData), Num(0), Num(tr.ID), Num(0), Blob(nil)}}, "wave-preview-data(4402) txid=3 args=[0x2004 0x0 0x388 blob[904 bytes]]"},
		{req(4, ReqWaveDetail, dmstData, tr.ID, 0), "wave-detail-data(4a02) txid=4 args=[0x2904 0x0 0x18 blob[24 bytes]]"},
		{req(5, ReqSeekIndex, dmstData, tr.ID), "type-4502 txid=5 args=[0x2504 0x0 0x644 blob[1604 bytes]]"},
		{req(6, ReqCuePointsExt, dmstData, tr.ID, 0), "cue-points-ext-data(4e02) txid=6 args=[0x2b04 0x1 0x0 blob[] 0x0]"},
		{&Message{TxID: 7, Type: ReqAnalysisTag2EX, Args: []Arg{Num(dmstData), Num(tr.ID), Num(0x36565750), Num(0x00584532)}}, "analysis-tag-data(4f02) txid=7 args=[0x2d04 0x32 0x0 blob[] 0x0]"},
		{req(8, ReqBeatGrid, dmstData, 424242), "beat-grid-data(4602) txid=8 args=[0x2204 0x32 0x0 blob[]]"},
	} {
		if got := one(c.req); got != c.want {
			t.Errorf("\n got %s\nwant %s", got, c.want)
		}
	}
	up := &Message{TxID: 9, Type: ReqUploadPreview, Args: []Arg{Num(dmstData), Num(0), Num(tr.ID), Num(0), Blob(nil), Num(900), Blob(make([]byte, 900))}}
	if got := one(up); !strings.HasPrefix(got, "success(4000)") {
		t.Errorf("0x2005 upload: %s", got)
	}
}

func TestPlayerPreviewUploadIsServedBack(t *testing.T) {
	lib := testLib()
	tr := lib.Tracks()[0]
	s := newSession(lib)
	s.Previews = previewcache.New(t.TempDir())
	preview := make([]byte, previewcache.Size)
	preview[0] = 0x13
	upload := &Message{TxID: 1, Type: ReqUploadPreview, Args: []Arg{Num(dmstData), Num(0), Num(tr.ID), Num(0), Blob(nil), Num(900), Blob(preview)}}
	// As the CDJ sends it: the empty blob after the 0 is omitted on the wire.
	got, _, err := Parse(upload.Encode())
	if err != nil {
		t.Fatal(err)
	}
	s.Handle(got)
	out := s.Handle(&Message{TxID: 2, Type: ReqWavePreview, Args: []Arg{Num(dmstData), Num(0), Num(tr.ID), Num(0), Blob(nil)}})
	if out[0].String() != "wave-preview-data(4402) txid=2 args=[0x2004 0x0 0x388 blob[904 bytes]]" || out[0].Args[3].Blob[0] != 0x13 {
		t.Fatalf("cached preview not served: %v", out[0])
	}
	// rekordbox analysis wins: uploads for analysed tracks are not stored.
	other := lib.Tracks()[1]
	s.Analysis = func(id uint32) *anlz.Analysis {
		if id == other.ID {
			return &anlz.Analysis{Source: anlz.SourceRekordbox}
		}
		return nil
	}
	upload.Args[2] = Num(other.ID)
	s.Handle(upload)
	if s.Previews.Get(previewKey(other)) != nil {
		t.Fatal("stored a player preview for a track with rekordbox analysis")
	}
}

// TestPreviewPriority: rekordbox analysis, then the player's uploaded
// preview, then generated waveforms.
func TestPreviewPriority(t *testing.T) {
	lib := testLib()
	tr := lib.Tracks()[0]
	s := newSession(lib)
	s.Previews = previewcache.New(t.TempDir())
	pwav := func(v byte) *anlz.File {
		body := make([]byte, 400)
		for i := range body {
			body[i] = v
		}
		return &anlz.File{Sections: []anlz.Section{sec("PWAV", 20, nil, body)}}
	}
	var current *anlz.Analysis
	s.Analysis = func(uint32) *anlz.Analysis { return current }
	first := func() byte {
		m := s.Handle(&Message{TxID: 1, Type: ReqWavePreview, Args: []Arg{Num(dmstData), Num(0), Num(tr.ID), Num(0), Blob(nil)}})[0]
		if len(m.Args) < 4 || len(m.Args[3].Blob) == 0 {
			return 0xff
		}
		return m.Args[3].Blob[0]
	}
	current = &anlz.Analysis{Source: anlz.SourceSlipmat, DAT: pwav(5)}
	if first() != 5 {
		t.Fatal("generated preview not served")
	}
	up := make([]byte, previewcache.Size)
	up[0] = 9
	s.Previews.Put(previewKey(tr), up)
	if first() != 9 {
		t.Fatal("player's own preview must beat generated waveforms")
	}
	current = &anlz.Analysis{Source: anlz.SourceRekordbox, DAT: pwav(3)}
	if first() != 3 {
		t.Fatal("rekordbox analysis must win")
	}
}
