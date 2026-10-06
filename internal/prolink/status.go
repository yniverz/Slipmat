// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

import (
	"encoding/binary"
	"fmt"
	"math"
	"net/netip"
)

// Port 50002 packet kinds.
const (
	KindMediaQuery     uint8 = 0x05 // [DS]
	KindMediaResponse  uint8 = 0x06 // [DS]
	KindCDJStatus      uint8 = 0x0a // [DS]
	KindRBHelloQuery   uint8 = 0x10 // CDJ -> rekordbox "who are you" [VN]
	KindRBHello        uint8 = 0x11 // rekordbox announce with host name [BL][VN]
	KindRBStatus       uint8 = 0x16 // rekordbox short status [VN]
	KindLoadTrack      uint8 = 0x19 // [DS]
	KindLoadTrackAck   uint8 = 0x1a // [DS]
	KindRatingRefresh  uint8 = 0x1b // [VN]
	KindLoadRejected   uint8 = 0x1c // "media unavailable" [VN]
	KindTrackRefresh   uint8 = 0x1d // [VN]
	KindMixerStatus    uint8 = 0x29 // mixer, and rekordbox [DS]
	KindMixerStatusNew uint8 = 0x30 // newer DJMs [VN]
	KindLoadSettings   uint8 = 0x34 // [DS]
	KindMySettingsReq  uint8 = 0x35 // [VN]
	KindMySettingsResp uint8 = 0x36 // [VN]
	KindMySettingsPut  uint8 = 0x37 // [VN]
	KindMySettingsAck  uint8 = 0x38 // [VN]
	KindLinkPing       uint8 = 0x46 // CDJ link keep-alive (~5 s) [VN]
	KindLinkActivate   uint8 = 0x47 // rekordbox link activation (+DEVSETTING) [VN]
	KindDevSettingsPut uint8 = 0x48 // [VN]
	KindDevSettingsAck uint8 = 0x49 // [VN]
	KindSettingsNotify uint8 = 0x4a // [VN]
	KindLighting       uint8 = 0x55 // rekordbox lighting / PSSI request [BL]
)

// StatusKindName names a port-50002 packet kind.
func StatusKindName(k uint8) string {
	if n, ok := statusKindNames[k]; ok {
		return n
	}
	return fmt.Sprintf("unknown-%#02x", k)
}

var statusKindNames = map[uint8]string{
	KindMediaQuery:     "media-query",
	KindMediaResponse:  "media-response",
	KindCDJStatus:      "cdj-status",
	KindRBHelloQuery:   "rb-hello-query",
	KindRBHello:        "rb-hello",
	KindRBStatus:       "rb-status",
	KindLoadTrack:      "load-track",
	KindLoadTrackAck:   "load-track-ack",
	KindRatingRefresh:  "rating-refresh",
	KindLoadRejected:   "load-rejected",
	KindTrackRefresh:   "track-refresh",
	KindMixerStatus:    "mixer-status",
	KindMixerStatusNew: "mixer-status-new",
	KindLoadSettings:   "load-settings",
	KindMySettingsReq:  "my-settings-req",
	KindMySettingsResp: "my-settings-resp",
	KindMySettingsPut:  "my-settings-put",
	KindMySettingsAck:  "my-settings-ack",
	KindLinkPing:       "link-ping",
	KindLinkActivate:   "link-activate",
	KindDevSettingsPut: "dev-settings-put",
	KindDevSettingsAck: "dev-settings-ack",
	KindSettingsNotify: "settings-notify",
	KindLighting:       "lighting",
}

// Status is a decoded port-50002 packet. Common header [DS]:
//
//	00-09 magic, 0a kind, 0b-1e name, 1f 0x01, 20 subtype,
//	21 device number, 22-23 remaining length (BE), 24.. payload
type Status struct {
	Kind    uint8
	Name    string
	Subtype uint8
	Device  uint8
	Length  uint16

	CDJ           *CDJStatus     // KindCDJStatus
	MediaQuery    *MediaQuery    // KindMediaQuery
	MediaResponse *MediaResponse // KindMediaResponse
	HostName      string         // KindRBHello
	TrackID       uint32         // KindLoadTrack / refresh triggers
	Slot          Slot           // KindLoadTrack, link-activate, settings packets
	Payload       []byte         // bytes from 0x24 (for unknown kinds)
}

// DecodeStatus parses a port-50002 packet. Unknown kinds decode the common
// header and keep the payload.
func DecodeStatus(b []byte) (*Status, error) {
	k, err := Kind(b)
	if err != nil {
		return nil, err
	}
	if err := need(b, 0x24); err != nil {
		return nil, err
	}
	s := &Status{
		Kind:    k,
		Name:    cString(b[0x0b:0x1f]),
		Subtype: b[0x20],
		Device:  b[0x21],
		Length:  binary.BigEndian.Uint16(b[0x22:0x24]),
		Payload: append([]byte(nil), b[0x24:]...),
	}
	switch k {
	case KindCDJStatus:
		c, err := decodeCDJStatus(b)
		if err != nil {
			return nil, err
		}
		s.CDJ = c
	case KindMediaQuery:
		if err := need(b, 0x30); err != nil {
			return nil, err
		}
		s.MediaQuery = &MediaQuery{IP: ip4(b[0x24:0x28]), Target: b[0x2b], Slot: Slot(b[0x2f])}
	case KindMediaResponse:
		if err := need(b, 0xac); err != nil {
			return nil, err
		}
		s.MediaResponse = &MediaResponse{
			Player:    b[0x27],
			Slot:      Slot(b[0x2b]),
			Name:      utf16BE(b[0x2c:0x6c]),
			Created:   utf16BE(b[0x6c:0x84]),
			Tracks:    binary.BigEndian.Uint16(b[0xa6:0xa8]),
			Color:     b[0xa8],
			TrackType: b[0xaa],
			Settings:  b[0xab],
		}
		if len(b) >= 0xb0 {
			s.MediaResponse.Playlists = binary.BigEndian.Uint16(b[0xae:0xb0])
		}
	case KindRBHello:
		if len(b) > 0x28 {
			s.HostName = utf16BE(b[0x28:])
		}
	case KindLoadTrack:
		if err := need(b, 0x30); err != nil {
			return nil, err
		}
		s.Slot = Slot(b[0x29])
		s.TrackID = binary.BigEndian.Uint32(b[0x2c:0x30])
	case KindTrackRefresh:
		if err := need(b, 0x30); err != nil {
			return nil, err
		}
		s.TrackID = binary.BigEndian.Uint32(b[0x2c:0x30])
	case KindRatingRefresh:
		if err := need(b, 0x2c); err != nil {
			return nil, err
		}
		s.TrackID = binary.BigEndian.Uint32(b[0x28:0x2c])
	case KindLinkActivate, KindSettingsNotify, KindMySettingsReq, KindMySettingsResp, KindMySettingsAck, KindDevSettingsAck:
		if len(b) > 0x25 {
			s.Slot = Slot(b[0x25])
		}
	}
	return s, nil
}

// String renders a one-line summary for logs.
func (s *Status) String() string {
	out := fmt.Sprintf("%s name=%q dev=%d sub=%#02x len=%d", StatusKindName(s.Kind), s.Name, s.Device, s.Subtype, s.Length)
	switch {
	case s.CDJ != nil:
		out += " " + s.CDJ.String()
	case s.MediaQuery != nil:
		out += fmt.Sprintf(" from=%s target=%d slot=%s", s.MediaQuery.IP, s.MediaQuery.Target, s.MediaQuery.Slot)
	case s.MediaResponse != nil:
		m := s.MediaResponse
		out += fmt.Sprintf(" player=%d slot=%s media=%q tracks=%d playlists=%d tracktype=%#02x settings=%#02x", m.Player, m.Slot, m.Name, m.Tracks, m.Playlists, m.TrackType, m.Settings)
	case s.Kind == KindRBHello:
		out += fmt.Sprintf(" host=%q", s.HostName)
	case s.Kind == KindLoadTrack:
		out += fmt.Sprintf(" slot=%s track=%d", s.Slot, s.TrackID)
	case s.Kind == KindTrackRefresh || s.Kind == KindRatingRefresh:
		out += fmt.Sprintf(" track=%d", s.TrackID)
	}
	return out
}

// MediaQuery asks a player which media is in a slot. [DS]
// Layout: 24-27 reply IP, 2b Dr (slot owner), 2f Sr (slot).
type MediaQuery struct {
	IP     netip.Addr
	Target uint8
	Slot   Slot
}

// CDJStatus holds the most useful fields of a CDJ status packet. [DS]
type CDJStatus struct {
	Activity   uint8   // A (0x27)
	SrcPlayer  uint8   // Dr (0x28): player the loaded track came from
	SrcSlot    Slot    // Sr (0x29)
	TrackType  uint8   // Tr (0x2a): 1 rekordbox, 2 unanalyzed, 5 CD
	TrackID    uint32  // rekordbox id (0x2c)
	PlayState  uint8   // P1 (0x7b)
	Firmware   string  // 0x7c-0x7f
	Flags      uint8   // F (0x89): bit6 play, bit5 master, bit4 sync, bit3 on-air
	Pitch      float64 // percent, from 0x8c-0x8f
	BPM        float64 // track BPM (0x92-0x93), NaN when no track
	Beat       uint32  // beat number (0xa0), when present
	BeatInBar  uint8   // 0xa6, when present
	PacketSize int
}

func decodeCDJStatus(b []byte) (*CDJStatus, error) {
	if err := need(b, 0xa0); err != nil {
		return nil, err
	}
	c := &CDJStatus{
		Activity:   b[0x27],
		SrcPlayer:  b[0x28],
		SrcSlot:    Slot(b[0x29]),
		TrackType:  b[0x2a],
		TrackID:    binary.BigEndian.Uint32(b[0x2c:0x30]),
		PlayState:  b[0x7b],
		Firmware:   cString(b[0x7c:0x80]),
		Flags:      b[0x89],
		Pitch:      pitchPercent(binary.BigEndian.Uint32(b[0x8c:0x90])),
		PacketSize: len(b),
	}
	if bpm := binary.BigEndian.Uint16(b[0x92:0x94]); bpm == 0xffff {
		c.BPM = math.NaN()
	} else {
		c.BPM = float64(bpm) / 100
	}
	if len(b) >= 0xa7 {
		c.Beat = binary.BigEndian.Uint32(b[0xa0:0xa4])
		c.BeatInBar = b[0xa6]
	}
	return c, nil
}

func pitchPercent(v uint32) float64 {
	return (float64(v) - 0x100000) * 100 / 0x100000
}

// PlayStateName names P1 values. [DS]
func PlayStateName(p uint8) string {
	switch p {
	case 0x00:
		return "no-track"
	case 0x02:
		return "loading"
	case 0x03:
		return "playing"
	case 0x04:
		return "looping"
	case 0x05:
		return "paused"
	case 0x06:
		return "cued"
	case 0x07:
		return "cue-play"
	case 0x08:
		return "cue-scratch"
	case 0x09:
		return "searching"
	case 0x0e:
		return "spun-down"
	case 0x11:
		return "ended"
	}
	return fmt.Sprintf("p1=%#02x", p)
}

func (c *CDJStatus) String() string {
	bpm := "--"
	if !math.IsNaN(c.BPM) {
		bpm = fmt.Sprintf("%.2f", c.BPM)
	}
	return fmt.Sprintf("state=%s track=%d from=%d/%s type=%d bpm=%s pitch=%+.2f%% beat=%d flags=%08b fw=%q size=%#x",
		PlayStateName(c.PlayState), c.TrackID, c.SrcPlayer, c.SrcSlot, c.TrackType, bpm, c.Pitch, c.Beat, c.Flags, c.Firmware, c.PacketSize)
}

// MediaResponse describes media mounted in a slot. [DS]
type MediaResponse struct {
	Player    uint8
	Slot      Slot
	Name      string
	Created   string
	Tracks    uint16
	Color     uint8
	TrackType uint8
	Settings  uint8 // 01 when My Settings are available
	Playlists uint16
}
