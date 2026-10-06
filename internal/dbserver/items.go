// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import (
	"unicode/utf16"
)

// Menu item types (argument 6). [DS][RB7]
const (
	ItemFolder      = 0x01
	ItemAlbum       = 0x02
	ItemTitle       = 0x04
	ItemGenre       = 0x06
	ItemArtist      = 0x07
	ItemPlaylist    = 0x08
	ItemRating      = 0x0a
	ItemDuration    = 0x0b
	ItemTempo       = 0x0d
	ItemLabel       = 0x0e
	ItemKey         = 0x0f
	ItemBitrate     = 0x10
	ItemYear        = 0x11
	ItemColorNone   = 0x13
	ItemComment     = 0x23
	ItemOrigArtist  = 0x28
	ItemRemixer     = 0x29
	ItemDateAdded   = 0x2e
	ItemPath        = 0x00 // track-info: file path (arg 0 = file size) [RB7]
	ItemFileType    = 0x2f // track-info: unknown, 1 for an MP3 [RB7]
	ItemMenuGenre   = 0x80
	ItemMenuArtist  = 0x81
	ItemMenuAlbum   = 0x82
	ItemMenuTrack   = 0x83
	ItemMenuPlaylst = 0x84
	ItemMenuBPM     = 0x85
	ItemMenuRating  = 0x86
	ItemMenuKey     = 0x8b
	ItemMenuAdded   = 0x8c
	ItemMenuFolder  = 0x90
	ItemMenuSearch  = 0x91
	ItemMenuHistory = 0x95
	ItemSortDefault = 0xa1
	ItemSortAlpha   = 0xa2
	ItemMenuMatch   = 0xaa
)

// FlagScoped marks track entries listed inside an album, artist or
// playlist (0 in the plain TRACK menu). [RB7]
const FlagScoped = 0x01000000

// Item is a menu item in rekordbox 7's 16-argument layout [RB7]:
//
//	0 parent   1 id   2 len(label1)   3 label1   4 len(label2)   5 label2
//	6 type     7 flags   8 artwork id   9 position   10..12 extra
//	13 len(label3)   14 label3   15 extra (BPM × 100 for track rows)
//
// Lengths are the UTF-16 byte length of the string including its NUL.
type Item struct {
	Parent   uint32
	ID       uint32
	Label1   string
	Label2   string
	Type     uint32
	Flags    uint32
	Artwork  uint32
	Position uint32
	A10      uint32
	A11      uint32
	A12      uint32
	Label3   string
	A15      uint32
}

func strBytes(s string) uint32 { return uint32(len(utf16.Encode([]rune(s)))+1) * 2 }

// Args returns the item's message arguments.
func (it Item) Args() []Arg {
	return []Arg{
		Num(it.Parent), Num(it.ID), Num(strBytes(it.Label1)), Str(it.Label1),
		Num(strBytes(it.Label2)), Str(it.Label2), Num(it.Type), Num(it.Flags),
		Num(it.Artwork), Num(it.Position), Num(it.A10), Num(it.A11), Num(it.A12),
		Num(strBytes(it.Label3)), Str(it.Label3), Num(it.A15),
	}
}

// menuLabel wraps a root/sort menu label in the U+FFFA/U+FFFB markers
// rekordbox uses for translatable menu names. [RB7]
func menuLabel(s string) string { return "￺" + s + "￻" }
