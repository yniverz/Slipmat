// SPDX-License-Identifier: GPL-3.0-or-later

package dbserver

import "fmt"

// Request message types. [DS] unless noted.
const (
	ReqSetup          = 0x0000
	ReqTeardown       = 0x0100
	ReqRootMenu       = 0x1000
	ReqGenreMenu      = 0x1001
	ReqArtistMenu     = 0x1002
	ReqAlbumMenu      = 0x1003
	ReqTrackMenu      = 0x1004
	ReqBPMMenu        = 0x1006
	ReqRatingMenu     = 0x1007
	ReqYearMenu       = 0x1008
	ReqLabelMenu      = 0x100a
	ReqColorMenu      = 0x100d
	ReqTimeMenu       = 0x1010
	ReqBitrateMenu    = 0x1011
	ReqHistoryMenu    = 0x1012
	ReqFilenameMenu   = 0x1013
	ReqKeyMenu        = 0x1014
	ReqArtistsByGenre = 0x1101
	ReqAlbumsByArtist = 0x1102
	ReqTracksByAlbum  = 0x1103
	ReqPlaylist       = 0x1105
	ReqTracksByBPM    = 0x1106
	ReqSearch         = 0x1300
	ReqTrackMetadata  = 0x2002
	ReqArtwork        = 0x2003
	ReqWavePreview    = 0x2004
	ReqFolder         = 0x2006
	ReqTrackInfo      = 0x2102 // unanalysed / CD tracks
	ReqCuePoints      = 0x2104
	ReqBeatGrid       = 0x2204
	ReqWaveDetail     = 0x2904
	ReqCuePointsExt   = 0x2b04
	ReqAnalysisTag    = 0x2c04
	ReqRenderMenu     = 0x3000
)

// Response message types. [DS]
const (
	RespSuccess     = 0x4000
	RespMenuHeader  = 0x4001
	RespArtwork     = 0x4002
	RespUnavailable = 0x4003
	RespMenuItem    = 0x4101
	RespMenuFooter  = 0x4201
	RespWavePreview = 0x4402
	RespBeatGrid    = 0x4602
	RespCuePoints   = 0x4702
	RespWaveDetail  = 0x4a02
	RespCueExt      = 0x4e02
	RespAnalysisTag = 0x4f02
)

var typeNames = map[uint16]string{
	ReqSetup: "setup", ReqTeardown: "teardown", ReqRootMenu: "root-menu",
	ReqGenreMenu: "genre-menu", ReqArtistMenu: "artist-menu", ReqAlbumMenu: "album-menu",
	ReqTrackMenu: "track-menu", ReqBPMMenu: "bpm-menu", ReqRatingMenu: "rating-menu",
	ReqYearMenu: "year-menu", ReqLabelMenu: "label-menu", ReqColorMenu: "color-menu",
	ReqTimeMenu: "time-menu", ReqBitrateMenu: "bitrate-menu", ReqHistoryMenu: "history-menu",
	ReqFilenameMenu: "filename-menu", ReqKeyMenu: "key-menu",
	ReqArtistsByGenre: "artists-by-genre", ReqAlbumsByArtist: "albums-by-artist",
	ReqTracksByAlbum: "tracks-by-album", ReqPlaylist: "playlist", ReqTracksByBPM: "tracks-by-bpm",
	ReqSearch: "search", ReqTrackMetadata: "track-metadata", ReqArtwork: "artwork",
	ReqWavePreview: "wave-preview", ReqFolder: "folder", ReqTrackInfo: "track-info",
	ReqCuePoints: "cue-points", ReqBeatGrid: "beat-grid", ReqWaveDetail: "wave-detail",
	ReqCuePointsExt: "cue-points-ext", ReqAnalysisTag: "analysis-tag", ReqRenderMenu: "render-menu",
	RespSuccess: "success", RespMenuHeader: "menu-header", RespArtwork: "artwork-data",
	RespUnavailable: "unavailable", RespMenuItem: "menu-item", RespMenuFooter: "menu-footer",
	RespWavePreview: "wave-preview-data", RespBeatGrid: "beat-grid-data", RespCuePoints: "cue-points-data",
	RespWaveDetail: "wave-detail-data", RespCueExt: "cue-points-ext-data", RespAnalysisTag: "analysis-tag-data",
}

// TypeName names a message type.
func TypeName(t uint16) string {
	if n, ok := typeNames[t]; ok {
		return fmt.Sprintf("%s(%04x)", n, t)
	}
	return fmt.Sprintf("type-%04x", t)
}
