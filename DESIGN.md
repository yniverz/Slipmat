# Slipmat — Design

Slipmat makes a computer appear on a Pioneer/AlphaTheta Pro DJ Link network as a
**rekordbox "Link Export" source**. CDJs see it under SOURCE, browse it, and load
and play tracks from a plain music folder, with no rekordbox running.

Primary target: **CDJ-3000** (firmware 3.2x), with the host on macOS (Apple
silicon) over Ethernet. Linux and Raspberry Pi should work as well, since the
build is cross-compiled pure Go.

> Pro DJ Link is undocumented. Every byte layout in this project is labelled
> with where it came from (Deep Symmetry docs, Vynull, or our own captures). We
> treat anything not confirmed by a capture of rekordbox 7 talking to a
> CDJ-3000 as a hypothesis.

## 1. Roles and ports

rekordbox 7 on macOS splits the work between the UI process and a helper,
`edb_streamd`. On the test Mac it listens on:

| Port            | Proto | Purpose                                                                 | Slipmat component |
|-----------------|-------|-------------------------------------------------------------------------|-------------------|
| 50000           | UDP   | Announce, device-number claim and keep-alive (broadcast)                | `device`          |
| 50001           | UDP   | Beats, absolute position (CDJ-3000), mixer on-air, sync (listen only)   | `monitor`         |
| 50002           | UDP   | Status, media query/response, rekordbox link handshake, settings        | `device`          |
| 12523           | TCP   | dbserver port discovery: answers with a 2-byte port                     | `dbserver`        |
| (dynamic)       | TCP   | dbserver: menus, metadata, waveforms, beat grids, cues                  | `dbserver`        |
| 50111           | UDP   | ONC RPC portmapper (rekordbox uses this instead of 111, so no root)     | `nfs`             |
| 2049            | UDP   | NFS v2, read-only: the CDJ reads the audio file itself                  | `nfs`             |
| (dynamic)       | UDP   | MOUNT v1 daemon                                                         | `nfs`             |

That is why Slipmat and rekordbox can't run on the same machine: both need
50000–50002, 12523, 50111 and 2049. Slipmat detects that and names the process
holding the port.

## 2. Components

```
cmd/slipmat            CLI: interfaces | monitor | decode <pcap> | serve
internal/logx          slog setup: trace/debug/info/warn/error, hex dumps
internal/netif         interface discovery, picker, saved choice, per-interface sockets
internal/prolink       PURE codec for UDP packets (50000/50001/50002): decode, encode, describe
internal/device        virtual-rekordbox state machine: claim → keep-alive → link handshake
internal/monitor       live listener: decodes and logs all traffic, keeps a peer table
internal/pcap          pcap/pcapng reader (Ethernet, NULL, RAW, SLL, macOS PKTAP) → UDP/TCP flows
internal/rpc           ONC RPC + XDR codec, portmapper (UDP 50111), MOUNT v1 (later NFS v2)
--- later milestones ---
internal/dbserver      dbserver message codec (pure) + TCP server (menus/metadata/analysis)
internal/rpc (nfs)     NFS v2 on top of the rpc package (read-only, sandboxed)
internal/library       track model + Library interface; folder scanner; rekordbox-USB importer
internal/anlz          ANLZ (.DAT/.EXT/.2EX) reader, file→track index, dbserver blob conversion
internal/previewcache  waveform previews uploaded by CDJ-3000s for unanalysed tracks
internal/pdb           export.pdb reader (to reuse an existing rekordbox USB export)
```

Rules:

- **Protocol code never touches the filesystem.** `dbserver` and `nfs` depend
  only on a `library.Library` interface (list folders and tracks, metadata,
  open a file by ID, fetch analysis blobs). Library implementations: the folder
  scanner, a rekordbox-USB-export reader, and later our own analysis.
- **Codecs are pure** (`[]byte` in, struct out, and back). Every codec has
  unit tests and round-trip tests, plus golden tests against bytes from real
  captures.
- **Never crash or confuse a deck.** Every decoder is bounds-checked and
  returns an error instead of panicking. We log unknown or malformed packets
  (with a hex dump at trace level) and don't reply. Each network handler
  recovers from panics. We never send a packet type we can't justify from a
  capture.

## 3. Data flow (rekordbox mode)

```
 Slipmat                                              CDJ-3000
 ───────                                              ────────
 UDP 50000  claim 0x00/0x02 bursts, keep-alive  ──bc──▶ sees "rekordbox" device (type 3)
 UDP 50002  ◀── 0x10 "who are you"                      (when SOURCE menu opened / link)
            0x11 announce (host name) ───────────────▶  shows our host under SOURCE
            ◀── 0x05 media query  /  0x06 media resp ─▶
            ◀── 0x46 link keep-alive (~5 s) / 0x16, 0x47 ▶  LINK established
 TCP 12523  ◀── "which port?"  → 2-byte port
 TCP dyn    ◀── greeting, context setup, menu requests → menu items (folders/tracks)
            ◀── track metadata / waveform / beat grid / cue requests → ANLZ-derived blobs
 UDP 50111  ◀── portmap GETPORT(mount, nfs)
 UDP mount  ◀── MNT /<export>  → root file handle
 UDP 2049   ◀── LOOKUP path components, GETATTR, READ (8 KiB chunks) → audio bytes
```

The track-info response tells the CDJ the file path, which it then reads over
NFS. Its title item ID picks the decoder (per Vynull: MP3=1, AAC=4, FLAC=5,
WAV=0x0b, AIFF=0x0c). **Unverified; check this against a capture.**

## 4. Device identity

- Device type 04 (rekordbox) in keep-alive byte 0x34, device number 17.
  The name and the 0x11 host name default to `Slipmat` (configurable).
- Claim sequence, cadence and handshake follow the rekordbox 7 capture
  exactly (see Findings).
- One rekordbox-type source per link: if a peer of type 3 is seen, Slipmat
  warns loudly and refuses to serve unless `--force`.

## 5. Network interface selection

The picker lists usable IPv4 interfaces with their macOS hardware-port names
("Wi-Fi", "USB 10/100/1G/2.5G LAN", …). It listens on UDP 50000 for about 3 s
and marks interfaces where Pro DJ Link keep-alives arrived. If exactly one
interface shows traffic, it's picked automatically. Otherwise you get a
numbered menu, and the choice is saved under the user config dir
(`~/Library/Application Support/slipmat/config.json` on macOS).
`--interface en13` (or a hardware-port name) overrides this. Sockets are
pinned to the chosen interface (macOS `IP_BOUND_IF`, Linux `SO_BINDTODEVICE`
where permitted), so we never announce on Wi-Fi by accident. Link-local
169.254/16 is fine.

## 6. Testing strategy

1. **Unit tests** for each encoder/decoder: field-level checks plus
   encode→decode round trips, including truncated and garbage input.
2. **Golden packets** in `testdata/`: small hex fixtures cut from real
   captures, so tests never need whole pcaps. Fixtures come from:
   - Deep Symmetry's CDJ-2000NXS hardware captures (dysentery, EPL-2.0; we
     extract packet bytes as data and don't vendor the files), and
   - **our own rekordbox 7 ↔ CDJ-3000 captures**. Raw pcaps stay out of git
     (`captures/` is ignored) because they contain library names.
3. **`slipmat decode <file.pcap>`** runs the same decoders as the live monitor
   over a capture: an annotated timeline of every packet and its dbserver
   messages. It's the main tool for comparing "what rekordbox did" with "what
   Slipmat does".
4. **Replay tests** (from milestone 2): feed every CDJ→rekordbox request found
   in a capture into our dbserver and NFS handlers, then compare our responses
   structurally (message types, item kinds, argument counts and types) with
   rekordbox's responses in that capture.
5. **Hardware checklists**: after each milestone, a list of steps on the real
   CDJ-3000 and the logs to send back.

Capture recipe: rekordbox runs on the same Mac, so there's no second machine
or mirror port. Run `scripts/capture.sh <name>` (tcpdump on the saved
interface, written to the git-ignored `captures/`). See TESTING.md.

## 7. Milestones

1. **Presence + monitor**: interface picker; `monitor` and `decode`;
   rekordbox-style claim, keep-alive and 50002 handshake so the CDJ lists us.
2. **Browsing**: dbserver: discovery, greeting, setup, root menu, folder
   browse, track list with title, artist, BPM and duration.
3. **Playback**: portmapper, MOUNT and NFSv2 read-only; track-info response
   with the right decoder ID; MP3/AAC/WAV/AIFF/FLAC natively.
4. **Analysis**: beat grid and waveforms (PWAV/PWV2/PWV3/PWV4/PWV5, and
   PWV6/PWV7 for the CDJ-3000) from existing ANLZ files, by reading
   rekordbox USB exports (PDB + ANLZ). Our own analysis comes later (ffmpeg
   decode).
5. **Extras**: cues, playlists, artwork, several decks at once, CDJ-USB mode
   (maybe).

## 8. Open questions (resolve with captures)

- The exact rekordbox 7 claim and keep-alive sequence and cadence (Vynull's
  description is inconsistent about the type byte at 0x21: 1 vs 2 for CDJ and
  mixer).
- Which packets make the CDJ-3000 show the source under SOURCE before LINK,
  and what makes LINK light up (Vynull: the NFS MOUNT EXPORT reply).
- The 0x46/0x47 link keep-alive: who starts it, and what happens if it lapses.
- dbserver: does the CDJ-3000 ask a rekordbox source for a different root
  menu, and which menus does it request (folder browse is our main target)?
- How the track-info title ID or path drives decoder choice, and what the CDJ
  does with unsupported formats.
- Whether the CDJ-3000 asks a rekordbox source for PWV6/PWV7 (3-band) via
  0x2c04, and whether it falls back to PWV4/PWV5 when we don't have them.
- NFS: export path naming, file-handle format expectations, READ size and
  retransmit behaviour; macOS UDP buffer sizes.

### Findings so far

From the rekordbox 7 ↔ 2× CDJ-3000 (fw 3.22) capture [RB7], 2026-10-06:

- **Device type is keep-alive byte 0x34** (01 CDJ, 02 mixer, 04 rekordbox).
  Byte 0x21 is `03` for both the CDJ-3000 and rekordbox 7 (`02` on NXS2).
- **Startup:** claim-1 ×3, then claim-2 ×6 rounds over devices
  {17, 18, 41–44}, one packet every 100 ms. No claim-3, no duplicate
  packets. After that: keep-alive every 2 s and a 0x29 status broadcast
  every ~100 ms. All broadcasts go out from fresh ephemeral source ports.
- **Handshake:** the first keep-alive triggers 0x10 from every CDJ, and
  rekordbox answers with 0x11 (host name). The CDJs then call portmap
  GETPORT(mount) on UDP 50111 and MOUNT EXPORT. **An empty export list keeps
  the source unavailable.** rekordbox sends an unprompted 0x16 to each CDJ,
  and the CDJs re-read the export list (`/` for `<ip>/<netmask>`), then
  GETPORT(nfs)=2049 and MNT `/`, which returns an all-zero 32-byte handle.
  Then comes the 0x05 media query (target 17, slot 4), answered with 0x06,
  and **one** 0x46 link ping, answered with 0x47. There are no periodic link
  pings.
- **Quit:** 0x16 to each CDJ. They re-read the (now empty) export list and
  UMNT. Then a 41-byte kind-0x08 broadcast carrying rekordbox's own number
  and IP (a "leaving" packet), and the keep-alives stop.
- **Browsing** uses TCP 12523 → dbserver on an ephemeral port. **NFS** reads
  happen only when a track is loaded.
- Every rekordbox packet above and the portmap/mount replies are reproduced
  byte for byte by our encoders (`encode_test.go`, `rpc_test.go`).

dbserver, from the same capture:

- Discovery: the player sends `00 00 00 0f "RemoteDBServer\0"` (split 2 + 17
  bytes) and gets back the 2-byte port of an ephemeral dbserver.
- **The argument-tag blob is exactly one tag per argument** (not padded to
  12). Menu items have **16 arguments**: parent, id, len+label1, len+label2,
  type, flags, artwork, position, three extras, len+label3, extra (BPM × 100
  in track rows).
- Setup is answered with type 0x0000 `[own device, client's 2nd arg]`.
  0x3007 and 0x3100 get an empty success. 0x1400 is the sort popup.
- Players pipeline requests on one connection and keep one pending menu per
  menu location (byte 2 of the first argument), so a metadata request
  (location 2) can be followed by artwork (8) before its render.
- "Not found" for data is `[request, 0x32, 0, (no blob)]`.
- The load sequence: 0x2102 track info (path, size, decoder id), 0x2002,
  0x2b04 cues, 0x2204 beat grid, 0x2c04 PQT2, 0x2504, 0x2d04 PWV6/PWV7 (.2EX),
  0x2004, 0x2c04 PWV5, 0x2904 and 0x2c04 PSSI, then NFS reads of the path.

NFS (loading), same capture:

- The player LOOKUPs the track-info path one component at a time from the
  zero root handle. **Names are UTF-16LE**, and pad bytes may be garbage.
  GETATTR, then READs of 16–32 KiB from several sockets in parallel. Each
  reply is one large UDP datagram, so the send buffer must be enlarged
  (macOS defaults to 9 KiB).
- Track info: the title item's id selects the decoder (MP3 = 1, FLAC = 5
  confirmed; AAC 4, WAV 0x0b, AIFF 0x0c from Vynull). The path item carries
  the file size in arg 0. Slipmat exports the music folder as `/`, so
  paths are relative to it and nothing else is reachable.

Analysis replies (verified byte for byte against rekordbox 7 for one
track, `anlz/TestAgainstRekordbox7`):

- 0x2c04/0x2d04 tag requests: LE length + the whole ANLZ section, padded to
  4 bytes. Reply `[req, 0, len, blob, 1]`.
- 0x2204 beat grid from PQTZ: 20-byte LE header (0x80000, n, 16n, 1, 1),
  16 bytes per beat (LE beat, BPM × 100, ms, eight 0xff). Reply ends in 0.
- 0x2004 preview: PWAV as (height, whiteness) pairs + PWV2 + 4 unknown bytes
  (we send zeros). 0x2904 detail: PWV3 behind an LE header (n, size, n,
  150, 1). 0x2504 (reply 0x4502): PVBR body byte-swapped to LE.
- rekordbox's own analysis folder records only `?/<file name>` in PPTH; USB
  exports record the full path.
- A CDJ-3000 playing an unanalysed track uploads its own 900-byte preview
  with 0x2005 every ~250 ms.

Slipmat on hardware:

- **A CDJ-3000 does not serve its own analysis of tracks loaded from a
  network source.** Requests for beat grid, waveforms or tags about the
  loaded track (slot 4, any track type, posing as player 2, 3 or 17) are
  answered with 0x4003 "unavailable". It answers only for its own USB/SD
  slots. It also computes no BPM or grid for our tracks (we report them as
  rekordbox-analysed); it only builds the preview and uploads it with
  0x2005. Slipmat caches that upload (`previewcache`), and on later loads the
  player takes it and stops re-analysing.
- The rekordbox-style 0x19 load command works on the CDJ-3000 (acked with
  0x1a). Loads sent before the player has linked are queued until it can
  reach the source.

- A blob argument after a number 0 is omitted from the wire. Deciding that
  by peeking at the next byte deadlocked live connections (the player sends
  nothing more until it has our reply), so track loads hung until other
  traffic arrived. The decoder now never looks ahead.

- Sending the post-hello 0x16 after a player had already linked stalled
  browsing for ~30 s. We now only send 0x16 when quitting.

From the CDJ-2000NXS captures [DSC]:

- Final-stage claims are 0x26 bytes. A real player's media response uses
  subtype 0x00 and reports `playlists=35`.
- The media query carries the target at 0x2b and the slot at 0x2f (Vynull
  reads the target from 0x27).
- Players send 50002 packets from ephemeral ports. Replies go to port 50002.

## 9. Licensing

Slipmat is **GPL-3.0-or-later**. That allows studying and adapting code from
Vynull (GPL-3.0) with attribution. Files that adapt Vynull code say so in a
header comment. Deep Symmetry projects (EPL-2.0) are used as documentation
only: we don't copy code, and crate-digger's `.ksy` files are not vendored.
pyrekordbox (MIT), prolink-connect (MIT), python-prodj-link (Apache-2.0) and
rekordcrate (MPL-2.0) are references only.
