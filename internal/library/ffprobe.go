// SPDX-License-Identifier: GPL-3.0-or-later

package library

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// FFprobe reads metadata with ffmpeg's ffprobe.
type FFprobe struct {
	Bin     string        // path to ffprobe ("" = look up in PATH)
	Timeout time.Duration // per file (default 20 s)
}

type ffOutput struct {
	Streams []struct {
		CodecType     string            `json:"codec_type"`
		CodecName     string            `json:"codec_name"`
		SampleRate    string            `json:"sample_rate"`
		BitsPerSample int               `json:"bits_per_sample"`
		BitsPerRaw    string            `json:"bits_per_raw_sample"`
		BitRate       string            `json:"bit_rate"`
		Tags          map[string]string `json:"tags"`
	} `json:"streams"`
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

// Probe implements Prober.
func (f FFprobe) Probe(ctx context.Context, path string) (*Probe, error) {
	bin := f.Bin
	if bin == "" {
		bin = "ffprobe"
	}
	to := f.Timeout
	if to == 0 {
		to = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", "--", path).Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return nil, fmt.Errorf("ffprobe: %s", strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, fmt.Errorf("ffprobe: %w", err)
	}
	return parseFFprobe(out)
}

func parseFFprobe(out []byte) (*Probe, error) {
	var o ffOutput
	if err := json.Unmarshal(out, &o); err != nil {
		return nil, fmt.Errorf("ffprobe output: %w", err)
	}
	si := -1
	for i, s := range o.Streams {
		if s.CodecType == "audio" {
			si = i
			break
		}
	}
	if si < 0 {
		return nil, fmt.Errorf("%w: no audio stream", ErrUnsupported)
	}
	s := o.Streams[si]
	p := &Probe{}
	switch codec := s.CodecName; {
	case codec == "mp3":
		p.Format = FormatMP3
	case codec == "aac":
		p.Format = FormatAAC
	case codec == "alac":
		// The CDJ-3000 plays ALAC, but we don't know the track-info decoder
		// id for it yet; skip rather than guess.
		return nil, fmt.Errorf("%w: ALAC is not supported yet", ErrUnsupported)
	case codec == "flac":
		p.Format = FormatFLAC
	case strings.HasPrefix(codec, "pcm_") && strings.Contains(o.Format.FormatName, "wav"):
		p.Format = FormatWAV
	case strings.HasPrefix(codec, "pcm_") && strings.Contains(o.Format.FormatName, "aiff"):
		p.Format = FormatAIFF
	default:
		return nil, fmt.Errorf("%w: codec %q in %q", ErrUnsupported, codec, o.Format.FormatName)
	}

	// Merge tags; stream tags (e.g. FLAC/Vorbis) override container tags.
	tags := map[string]string{}
	for k, v := range o.Format.Tags {
		tags[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	for k, v := range s.Tags {
		tags[strings.ToLower(k)] = strings.TrimSpace(v)
	}
	first := func(keys ...string) string {
		for _, k := range keys {
			if v := tags[k]; v != "" {
				return v
			}
		}
		return ""
	}
	p.Title = first("title")
	p.Artist = first("artist", "album_artist")
	p.Album = first("album")
	p.Genre = first("genre")
	p.Key = first("initialkey", "tkey", "key")
	p.Comment = first("comment", "description")
	p.Label = first("label", "publisher", "organization")
	if y := first("date", "year", "tyer", "tdrc"); len(y) >= 4 {
		p.Year, _ = strconv.Atoi(y[:4])
	}
	if b := first("tbpm", "bpm", "tempo"); b != "" {
		if v, err := strconv.ParseFloat(strings.ReplaceAll(b, ",", "."), 64); err == nil && v > 0 && v < 1000 {
			p.BPM100 = uint32(math.Round(v * 100))
		}
	}
	if d, err := strconv.ParseFloat(o.Format.Duration, 64); err == nil && d > 0 {
		p.Duration = uint32(math.Round(d))
	}
	br := s.BitRate
	if br == "" {
		br = o.Format.BitRate
	}
	if v, err := strconv.ParseFloat(br, 64); err == nil {
		p.Bitrate = uint32(math.Round(v / 1000))
	}
	if v, err := strconv.Atoi(s.SampleRate); err == nil {
		p.SampleRate = uint32(v)
	}
	p.SampleDepth = uint32(s.BitsPerSample)
	if p.SampleDepth == 0 {
		if v, err := strconv.Atoi(s.BitsPerRaw); err == nil {
			p.SampleDepth = uint32(v)
		}
	}
	return p, nil
}
