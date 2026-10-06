// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"fmt"
	"log/slog"

	"github.com/yniverz/slipmat/internal/library"
)

// Request types seen from a CDJ-3000 talking to rekordbox 7 that the [DS]
// table doesn't name. [RB7]
const (
	ReqSortMenu             = 0x1400 // M=05: the sort-order popup
	ReqPrepare3007          = 0x3007 // [DMST(M=08), 0] at connect; rekordbox answers success(0)
	ReqPrepare3100          = 0x3100 // [DMST, id, 0, 1] before (re)listing; rekordbox answers success(0)
	ReqTracksForArtistAlbum = 0x1202
)

// StatusNotFound is the second argument of a data response when the data
// doesn't exist (e.g. no artwork). [RB7]
const StatusNotFound = 0x32

// DMST is the first argument of most requests: requesting device, menu
// location, slot and track type, one byte each. [DS]
type DMST struct{ Device, Menu, Slot, Type uint8 }

func parseDMST(v uint32) DMST {
	return DMST{uint8(v >> 24), uint8(v >> 16), uint8(v >> 8), uint8(v)}
}

// Session holds one connection's state: the menu prepared for each menu
// location, waiting to be rendered. The CDJ pipelines requests (e.g. a
// metadata request for location 2 and artwork for location 8), so state
// is kept per location. [RB7]
type Session struct {
	Lib    func() *library.Library
	Device uint8 // our device number
	Log    *slog.Logger

	menus map[uint8][]Item
	// Closed is set after a teardown request.
	Closed bool
}

// NewSession starts a session.
func NewSession(lib func() *library.Library, dev uint8, log *slog.Logger) *Session {
	return &Session{Lib: lib, Device: dev, Log: log, menus: map[uint8][]Item{}}
}

func success(req *Message, n uint32) *Message {
	return &Message{TxID: req.TxID, Type: RespSuccess, Args: []Arg{Num(uint32(req.Type)), Num(n)}}
}

// Handle answers one request. It never panics on bad input: unknown
// requests get an empty success so the player doesn't hang.
func (s *Session) Handle(m *Message) []*Message {
	if len(m.Args) == 0 && m.Type != ReqTeardown {
		s.Log.Warn("request without arguments", "msg", m.String())
		return []*Message{success(m, 0)}
	}
	d := parseDMST(m.NumArg(0))
	lib := s.Lib()
	switch m.Type {
	case ReqSetup:
		// rekordbox 7 echoes the message type (0) with its own device number
		// and the client's second argument. [RB7]
		args := []Arg{Num(uint32(s.Device))}
		if len(m.Args) > 1 {
			args = append(args, m.Args[1])
		}
		return []*Message{{TxID: m.TxID, Type: ReqSetup, Args: args}}
	case ReqTeardown:
		s.Closed = true
		return nil
	case ReqPrepare3007, ReqPrepare3100:
		return []*Message{success(m, 0)}
	case ReqRenderMenu:
		return s.render(m, d)
	case ReqRootMenu:
		return s.prepare(m, d, rootMenu())
	case ReqSortMenu:
		return s.prepare(m, d, sortMenu())
	case ReqArtistMenu:
		var items []Item
		for _, a := range lib.Artists() {
			items = append(items, Item{ID: a.ID, Label1: a.Name, Type: ItemArtist})
		}
		return s.prepare(m, d, items)
	case ReqAlbumMenu:
		return s.prepare(m, d, albumItems(lib.Albums()))
	case ReqTrackMenu:
		return s.prepare(m, d, trackRows(lib.Tracks(), 0, false))
	case ReqAlbumsByArtist:
		return s.prepare(m, d, albumItems(lib.AlbumsByArtist(m.NumArg(2))))
	case ReqTracksByAlbum:
		var items []Item
		if al, ok := lib.Album(m.NumArg(2)); ok {
			items = trackRows(lib.EntityTracks(al), FlagScoped, false)
		}
		return s.prepare(m, d, items)
	case ReqTracksForArtistAlbum:
		return s.prepare(m, d, trackRows(tracksForArtistAlbum(lib, m.NumArg(2), m.NumArg(3)), FlagScoped, false))
	case ReqPlaylist:
		return s.prepare(m, d, playlistItems(lib, m.NumArg(2), m.NumArg(3) == 1))
	case ReqTrackMetadata:
		t, ok := lib.Track(m.NumArg(1))
		if !ok {
			s.menus[d.Menu] = nil
			return []*Message{success(m, 0xffffffff)}
		}
		return s.prepare(m, d, metadataItems(lib, t))
	case ReqArtwork:
		return []*Message{notFound(m, RespArtwork, false)}
	case ReqAnalysisTag, 0x2d04:
		return []*Message{notFound(m, RespAnalysisTag, true)}
	case ReqWavePreview, ReqWaveDetail, ReqBeatGrid, ReqCuePoints, ReqCuePointsExt, 0x2504:
		s.Log.Debug("analysis data not available yet", "req", TypeName(m.Type))
		return []*Message{notFound(m, analysisResponse[m.Type], false)}
	}
	s.Log.Warn("unsupported request; answering with an empty result", "msg", m.String())
	return []*Message{success(m, 0)}
}

var analysisResponse = map[uint16]uint16{
	ReqWavePreview: RespWavePreview, ReqWaveDetail: RespWaveDetail, ReqBeatGrid: RespBeatGrid,
	ReqCuePoints: RespCuePoints, ReqCuePointsExt: RespCueExt, 0x2504: 0x4502,
}

// notFound builds rekordbox's "no such data" reply: [request, 0x32, 0,
// (empty blob, omitted on the wire)] plus a trailing 0 for analysis tags. [RB7][VN]
func notFound(req *Message, resp uint16, trailing bool) *Message {
	args := []Arg{Num(uint32(req.Type)), Num(StatusNotFound), Num(0), Blob(nil)}
	if trailing {
		args = append(args, Num(0))
	}
	return &Message{TxID: req.TxID, Type: resp, Args: args}
}

// prepare stores a menu for later rendering and reports its size.
func (s *Session) prepare(m *Message, d DMST, items []Item) []*Message {
	s.menus[d.Menu] = items
	return []*Message{success(m, uint32(len(items)))}
}

// render sends a window of the prepared menu: header, items, footer.
// Arguments: [DMST, offset, limit, 0, total, 0xc, 1, 0]. [DS][RB7]
func (s *Session) render(m *Message, d DMST) []*Message {
	items := s.menus[d.Menu]
	off, limit := int(m.NumArg(1)), int(m.NumArg(2))
	if off > len(items) {
		off = len(items)
	}
	end := off + limit
	if limit <= 0 || end > len(items) || end < off {
		end = len(items)
	}
	out := []*Message{{TxID: m.TxID, Type: RespMenuHeader, Args: []Arg{Num(1), Num(0)}}}
	for _, it := range items[off:end] {
		out = append(out, &Message{TxID: m.TxID, Type: RespMenuItem, Args: it.Args()})
	}
	return append(out, &Message{TxID: m.TxID, Type: RespMenuFooter})
}

// rootMenu lists the categories we support. IDs and types match rekordbox 7;
// the player requests 0x1000+ID for most of them. [RB7]
func rootMenu() []Item {
	cat := func(id, typ uint32, name string) Item {
		return Item{ID: id, Label1: menuLabel(name), Type: typ}
	}
	return []Item{
		cat(0x02, ItemMenuArtist, "ARTIST"),
		cat(0x03, ItemMenuAlbum, "ALBUM"),
		cat(0x04, ItemMenuTrack, "TRACK"),
		cat(0x05, ItemMenuPlaylst, "PLAYLIST"),
	}
}

// sortMenu is rekordbox 7's sort popup, verbatim. [RB7]
func sortMenu() []Item {
	s := func(id, typ uint32, name string) Item {
		return Item{ID: id, Label1: menuLabel(name), Type: typ}
	}
	return []Item{
		s(0x00, ItemSortDefault, "DEFAULT"),
		s(0x01, ItemSortAlpha, "ALPHABET"),
		s(0x02, ItemMenuArtist, "ARTIST"),
		s(0x03, ItemMenuAlbum, "ALBUM"),
		s(0x04, ItemMenuBPM, "BPM"),
		s(0x05, ItemMenuRating, "RATING"),
		s(0x0c, ItemMenuKey, "KEY"),
	}
}

func albumItems(albums []*library.Entity) []Item {
	items := make([]Item, 0, len(albums))
	for _, a := range albums {
		items = append(items, Item{ID: a.ID, Label1: a.Name, Type: ItemAlbum, Artwork: a.ID})
	}
	return items
}

// trackRow is a track entry in a list: title plus key and BPM columns. [RB7]
func trackRow(t *library.Track, flags uint32, pos uint32) Item {
	it := Item{ID: t.ID, Label1: t.Title, Type: ItemTitle, Flags: flags, Artwork: t.ID, Position: pos, A10: 0x100, A15: t.BPM100}
	if t.Key != "" {
		it.A11, it.A12, it.Label3 = 0x05, t.KeyID, t.Key
	}
	return it
}

func trackRows(ts []*library.Track, flags uint32, numbered bool) []Item {
	items := make([]Item, 0, len(ts))
	for i, t := range ts {
		pos := uint32(0)
		if numbered {
			pos = uint32(i + 1)
		}
		items = append(items, trackRow(t, flags, pos))
	}
	return items
}

func tracksForArtistAlbum(lib *library.Library, artist, album uint32) []*library.Track {
	var out []*library.Track
	if al, ok := lib.Album(album); ok && album != 0xffffffff {
		for _, t := range lib.EntityTracks(al) {
			if artist == 0xffffffff || t.ArtistID == artist {
				out = append(out, t)
			}
		}
		return out
	}
	if a, ok := lib.Artist(artist); ok {
		return lib.EntityTracks(a)
	}
	return nil
}

// Folder tree as playlists: a directory with sub-directories is a folder;
// a directory without is a playlist of its tracks. A folder that also holds
// tracks directly gets an extra playlist entry (same ID, isFolder=0)
// listing them.
func playlistItems(lib *library.Library, id uint32, isFolder bool) []Item {
	f, ok := lib.Folder(id)
	if !ok {
		return nil
	}
	if !isFolder {
		return trackRows(lib.FolderTracks(f), FlagScoped, true)
	}
	var items []Item
	pos := uint32(1)
	if len(f.Tracks) > 0 && (len(f.Folders) > 0 || id == library.RootFolderID) {
		name := "▶ " + f.Name
		if id == library.RootFolderID {
			name = "▶ (tracks in the music folder)"
		}
		items = append(items, Item{Parent: id, ID: f.ID, Label1: name, Type: ItemPlaylist, Position: pos})
		pos++
	}
	for _, cid := range f.Folders {
		c, _ := lib.Folder(cid)
		typ := uint32(ItemPlaylist)
		if len(c.Folders) > 0 {
			typ = ItemFolder
		}
		items = append(items, Item{Parent: id, ID: c.ID, Label1: c.Name, Type: typ, Position: pos})
		pos++
	}
	return items
}

// metadataItems is the 16-entry track info popup, in rekordbox 7's order. [RB7]
func metadataItems(lib *library.Library, t *library.Track) []Item {
	added := t.ModTime.Format("2006-01-02")
	return []Item{
		trackRow(t, 0, 0),
		{Parent: 1, ID: t.ArtistID, Label1: t.Artist, Type: ItemArtist},
		{Parent: 1, ID: t.AlbumID, Label1: t.Album, Type: ItemAlbum},
		{ID: t.Duration, Type: ItemDuration},
		{ID: t.BPM100, Type: ItemTempo},
		{Parent: 1, ID: t.KeyID, Label1: t.Key, Type: ItemKey},
		{ID: 0, Type: ItemRating},
		{ID: 0, Type: ItemColorNone},
		{ID: t.GenreID, Label1: t.Genre, Type: ItemGenre},
		{Parent: 1, ID: t.ID, Label1: added, Type: ItemDateAdded},
		{ID: t.ID, Label1: t.Comment, Type: ItemComment},
		{ID: t.Bitrate, Type: ItemBitrate},
		{ID: uint32(t.Year), Type: ItemYear},
		{ID: 0, Label1: t.Label, Type: ItemLabel},
		{ID: 0, Type: ItemOrigArtist},
		{ID: 0, Type: ItemRemixer},
	}
}

func (d DMST) String() string {
	return fmt.Sprintf("dev=%d menu=%d slot=%d type=%d", d.Device, d.Menu, d.Slot, d.Type)
}
