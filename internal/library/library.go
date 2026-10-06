// SPDX-License-Identifier: GPL-3.0-or-later

// Package library models a music collection independently of any protocol:
// tracks with their metadata, the artist/album/genre/key entities derived
// from them, and the folder tree they live in. The dbserver and NFS code
// only see it through these types.
package library

import (
	"hash/fnv"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Format is an audio container/codec family the CDJ-3000 can play.
type Format string

const (
	FormatMP3  Format = "mp3"
	FormatAAC  Format = "aac" // .m4a/.mp4/.aac with AAC audio
	FormatALAC Format = "alac"
	FormatFLAC Format = "flac"
	FormatWAV  Format = "wav"
	FormatAIFF Format = "aiff"
)

// Track is one audio file.
type Track struct {
	ID       uint32
	Path     string // absolute path on this machine
	RelPath  string // path relative to the library root (slash-separated)
	FolderID uint32
	Size     int64
	ModTime  time.Time
	Format   Format

	Title, Artist, Album, Genre, Key, Comment, Label string
	Year                                             int
	BPM100                                           uint32 // BPM × 100 (0 = unknown)
	Duration                                         uint32 // seconds
	Bitrate                                          uint32 // kbit/s
	SampleRate                                       uint32 // Hz
	SampleDepth                                      uint32 // bits (lossless formats)

	ArtistID, AlbumID, GenreID, KeyID uint32
}

// Entity is a named item derived from track tags (artist, album, genre, key).
type Entity struct {
	ID       uint32
	Name     string
	ArtistID uint32 // albums: album artist (0 when mixed/unknown)
	Tracks   []uint32

	ident string // identity used for hashing and collision checks
}

// Folder is a directory in the library.
type Folder struct {
	ID       uint32
	ParentID uint32 // 0 for top-level folders
	Name     string
	RelPath  string
	Folders  []uint32 // sub-folders, sorted by name
	Tracks   []uint32 // tracks directly inside, sorted by file name
}

// RootFolderID is the library root.
const RootFolderID = 0

// Library is an immutable snapshot of a scanned collection.
type Library struct {
	Root    string
	tracks  map[uint32]*Track
	order   []uint32 // tracks sorted by title
	artists entities
	albums  entities
	genres  entities
	keys    entities
	folders map[uint32]*Folder
}

type entities struct {
	byID  map[uint32]*Entity
	order []uint32 // sorted by name
}

// Track returns a track by ID.
func (l *Library) Track(id uint32) (*Track, bool) {
	t, ok := l.tracks[id]
	return t, ok
}

// Tracks returns all tracks sorted by title.
func (l *Library) Tracks() []*Track { return l.list(l.order) }

// TrackCount returns the number of tracks.
func (l *Library) TrackCount() int { return len(l.order) }

func (l *Library) list(ids []uint32) []*Track {
	out := make([]*Track, 0, len(ids))
	for _, id := range ids {
		if t := l.tracks[id]; t != nil {
			out = append(out, t)
		}
	}
	return out
}

// Artists, Albums, Genres and Keys return entities sorted by name.
func (l *Library) Artists() []*Entity { return l.artists.all() }
func (l *Library) Albums() []*Entity  { return l.albums.all() }
func (l *Library) Genres() []*Entity  { return l.genres.all() }
func (l *Library) Keys() []*Entity    { return l.keys.all() }

// Artist, Album, Genre and Key look up an entity by ID.
func (l *Library) Artist(id uint32) (*Entity, bool) { return l.artists.get(id) }
func (l *Library) Album(id uint32) (*Entity, bool)  { return l.albums.get(id) }
func (l *Library) Genre(id uint32) (*Entity, bool)  { return l.genres.get(id) }
func (l *Library) Key(id uint32) (*Entity, bool)    { return l.keys.get(id) }

func (e *entities) all() []*Entity {
	out := make([]*Entity, 0, len(e.order))
	for _, id := range e.order {
		out = append(out, e.byID[id])
	}
	return out
}

func (e *entities) get(id uint32) (*Entity, bool) {
	x, ok := e.byID[id]
	return x, ok
}

// EntityTracks returns an entity's tracks sorted by title.
func (l *Library) EntityTracks(e *Entity) []*Track { return l.list(e.Tracks) }

// AlbumsByArtist returns albums containing tracks by the artist.
func (l *Library) AlbumsByArtist(artistID uint32) []*Entity {
	seen := map[uint32]bool{}
	var out []*Entity
	if a, ok := l.artists.get(artistID); ok {
		for _, tid := range a.Tracks {
			if t := l.tracks[tid]; t != nil && t.AlbumID != 0 && !seen[t.AlbumID] {
				seen[t.AlbumID] = true
				out = append(out, l.albums.byID[t.AlbumID])
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return lessFold(out[i].Name, out[j].Name) })
	return out
}

// Folder returns a folder by ID (RootFolderID for the library root).
func (l *Library) Folder(id uint32) (*Folder, bool) {
	f, ok := l.folders[id]
	return f, ok
}

// FolderCount returns the number of folders below the root.
func (l *Library) FolderCount() int { return len(l.folders) - 1 }

// FolderTracks returns a folder's own tracks.
func (l *Library) FolderTracks(f *Folder) []*Track { return l.list(f.Tracks) }

// Builder assembles a Library from scanned tracks.
type Builder struct {
	lib *Library
}

// NewBuilder starts a library rooted at root.
func NewBuilder(root string) *Builder {
	l := &Library{
		Root:    root,
		tracks:  map[uint32]*Track{},
		folders: map[uint32]*Folder{RootFolderID: {ID: RootFolderID, Name: filepath.Base(root)}},
	}
	for _, e := range []*entities{&l.artists, &l.albums, &l.genres, &l.keys} {
		e.byID = map[uint32]*Entity{}
	}
	return &Builder{lib: l}
}

// Add inserts a track; RelPath, Path and metadata must be filled in. The
// track's IDs are assigned here.
func (b *Builder) Add(t *Track) {
	l := b.lib
	t.ID = uniqueID(hashID("track:"+t.RelPath), func(id uint32) bool { _, used := l.tracks[id]; return used })
	if strings.TrimSpace(t.Title) == "" {
		base := filepath.Base(t.RelPath)
		t.Title = strings.TrimSuffix(base, filepath.Ext(base))
	}
	l.tracks[t.ID] = t
	t.FolderID = b.folder(filepath.ToSlash(filepath.Dir(t.RelPath)))
	l.folders[t.FolderID].Tracks = append(l.folders[t.FolderID].Tracks, t.ID)
	t.ArtistID = addEntity(&l.artists, "artist:", t.Artist, t.ID)
	t.GenreID = addEntity(&l.genres, "genre:", t.Genre, t.ID)
	t.KeyID = addEntity(&l.keys, "key:", t.Key, t.ID)
	if t.Album != "" {
		// An album is a name within a folder: compilations stay together,
		// same-named albums in different folders stay apart.
		t.AlbumID = addEntity(&l.albums, "album:"+filepath.ToSlash(filepath.Dir(t.RelPath))+"\x00", t.Album, t.ID)
		al := l.albums.byID[t.AlbumID]
		al.Name = t.Album
		if len(al.Tracks) == 1 {
			al.ArtistID = t.ArtistID
		} else if al.ArtistID != t.ArtistID {
			al.ArtistID = 0
		}
	}
}

// folder returns the ID of the folder for a slash-separated relative
// directory, creating it and its parents as needed.
func (b *Builder) folder(rel string) uint32 {
	if rel == "." || rel == "" {
		return RootFolderID
	}
	l := b.lib
	id := hashID("folder:" + rel)
	for {
		f, ok := l.folders[id]
		if !ok {
			break
		}
		if f.RelPath == rel {
			return id
		}
		id++ // collision with another path
		if id == 0 {
			id = 1
		}
	}
	parent := b.folder(filepath.ToSlash(filepath.Dir(rel)))
	l.folders[id] = &Folder{ID: id, ParentID: parent, Name: filepath.Base(rel), RelPath: rel}
	l.folders[parent].Folders = append(l.folders[parent].Folders, id)
	return id
}

// Build finalises sort orders and returns the library.
func (b *Builder) Build() *Library {
	l := b.lib
	byTitle := func(ids []uint32) {
		sort.SliceStable(ids, func(i, j int) bool {
			a, c := l.tracks[ids[i]], l.tracks[ids[j]]
			if !strings.EqualFold(a.Title, c.Title) {
				return lessFold(a.Title, c.Title)
			}
			return a.RelPath < c.RelPath
		})
	}
	l.order = make([]uint32, 0, len(l.tracks))
	for id := range l.tracks {
		l.order = append(l.order, id)
	}
	byTitle(l.order)
	for _, e := range []*entities{&l.artists, &l.albums, &l.genres, &l.keys} {
		e.order = e.order[:0]
		for id, x := range e.byID {
			e.order = append(e.order, id)
			byTitle(x.Tracks)
		}
		sort.Slice(e.order, func(i, j int) bool { return lessFold(e.byID[e.order[i]].Name, e.byID[e.order[j]].Name) })
	}
	for _, f := range l.folders {
		sort.Slice(f.Folders, func(i, j int) bool { return lessFold(l.folders[f.Folders[i]].Name, l.folders[f.Folders[j]].Name) })
		sort.Slice(f.Tracks, func(i, j int) bool { return l.tracks[f.Tracks[i]].RelPath < l.tracks[f.Tracks[j]].RelPath })
	}
	return l
}

func addEntity(e *entities, ns, name string, track uint32) uint32 {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0
	}
	ident := ns + strings.ToLower(name)
	id := hashID(ident)
	for {
		x, ok := e.byID[id]
		if !ok {
			e.byID[id] = &Entity{ID: id, Name: name, Tracks: []uint32{track}, ident: ident}
			return id
		}
		if x.ident == ident {
			x.Tracks = append(x.Tracks, track)
			return id
		}
		id = uniqueID(id+1, func(uint32) bool { return false })
	}
}

// hashID derives a stable, non-zero 32-bit ID (FNV-1a). Stable IDs let the
// players keep referring to the same track across restarts.
func hashID(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	id := h.Sum32()
	if id == 0 || id == 0xffffffff {
		id = 1
	}
	return id
}

func uniqueID(id uint32, used func(uint32) bool) uint32 {
	for used(id) || id == 0 || id == 0xffffffff {
		id++
	}
	return id
}

func lessFold(a, b string) bool {
	la, lb := strings.ToLower(a), strings.ToLower(b)
	if la != lb {
		return la < lb
	}
	return a < b
}
