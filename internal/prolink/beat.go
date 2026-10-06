// SPDX-License-Identifier: GPL-3.0-or-later

package prolink

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Port 50001 packet kinds. [DS]
const (
	KindFaderStart     uint8 = 0x02
	KindChannelsOnAir  uint8 = 0x03
	KindAbsPosition    uint8 = 0x0b // CDJ-3000 only, every ~30 ms while a track is loaded
	KindMasterHandoff  uint8 = 0x26
	KindMasterResponse uint8 = 0x27
	KindBeat           uint8 = 0x28
	KindSyncControl    uint8 = 0x2a
)

// BeatKindName names a port-50001 packet kind.
func BeatKindName(k uint8) string {
	switch k {
	case KindFaderStart:
		return "fader-start"
	case KindChannelsOnAir:
		return "channels-on-air"
	case KindAbsPosition:
		return "abs-position"
	case KindMasterHandoff:
		return "master-handoff"
	case KindMasterResponse:
		return "master-response"
	case KindBeat:
		return "beat"
	case KindSyncControl:
		return "sync-control"
	}
	return fmt.Sprintf("unknown-%#02x", k)
}

// BeatPacket is a decoded port-50001 packet (same header shape as 50002).
type BeatPacket struct {
	Kind   uint8
	Name   string
	Device uint8

	// KindBeat
	BPM       float64 // track BPM (0x5a), NaN when unknown
	Pitch     float64 // percent (0x54)
	BeatInBar uint8   // 0x5c

	// KindAbsPosition
	TrackLength uint32  // seconds
	PlayheadMS  uint32  // milliseconds
	EffBPM      float64 // effective BPM (pitch applied), NaN when unknown
}

// DecodeBeat parses a port-50001 packet.
func DecodeBeat(b []byte) (*BeatPacket, error) {
	k, err := Kind(b)
	if err != nil {
		return nil, err
	}
	if err := need(b, 0x22); err != nil {
		return nil, err
	}
	p := &BeatPacket{Kind: k, Name: cString(b[0x0b:0x1f]), Device: b[0x21]}
	switch k {
	case KindBeat:
		if err := need(b, 0x5d); err != nil {
			return nil, err
		}
		p.Pitch = pitchPercent(binary.BigEndian.Uint32(b[0x54:0x58]))
		if v := binary.BigEndian.Uint16(b[0x5a:0x5c]); v == 0xffff {
			p.BPM = math.NaN()
		} else {
			p.BPM = float64(v) / 100
		}
		p.BeatInBar = b[0x5c]
	case KindAbsPosition:
		if err := need(b, 0x3c); err != nil {
			return nil, err
		}
		p.TrackLength = binary.BigEndian.Uint32(b[0x24:0x28])
		p.PlayheadMS = binary.BigEndian.Uint32(b[0x28:0x2c])
		p.Pitch = float64(int32(binary.BigEndian.Uint32(b[0x2c:0x30]))) / 100
		if v := binary.BigEndian.Uint32(b[0x38:0x3c]); v == 0xffffffff {
			p.EffBPM = math.NaN()
		} else {
			p.EffBPM = float64(v) / 10
		}
	}
	return p, nil
}

func (p *BeatPacket) String() string {
	s := fmt.Sprintf("%s name=%q dev=%d", BeatKindName(p.Kind), p.Name, p.Device)
	switch p.Kind {
	case KindBeat:
		s += fmt.Sprintf(" bpm=%.2f pitch=%+.2f%% beat=%d", p.BPM, p.Pitch, p.BeatInBar)
	case KindAbsPosition:
		s += fmt.Sprintf(" pos=%.3fs/%ds bpm=%.1f pitch=%+.2f%%", float64(p.PlayheadMS)/1000, p.TrackLength, p.EffBPM, p.Pitch)
	}
	return s
}

// Describe decodes any Pro DJ Link UDP payload received on port and returns a
// one-line description, or an error for malformed/unknown-port packets.
func Describe(port uint16, b []byte) (string, error) {
	switch port {
	case PortAnnounce:
		a, err := DecodeAnnounce(b)
		if err != nil {
			return "", err
		}
		return a.String(), nil
	case PortBeat:
		p, err := DecodeBeat(b)
		if err != nil {
			return "", err
		}
		return p.String(), nil
	case PortStatus:
		s, err := DecodeStatus(b)
		if err != nil {
			return "", err
		}
		return s.String(), nil
	case PortTouchAudio:
		k, err := Kind(b)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("touch-audio kind=%#02x len=%d", k, len(b)), nil
	}
	return "", fmt.Errorf("prolink: not a Pro DJ Link port: %d", port)
}
