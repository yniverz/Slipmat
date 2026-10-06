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
--- later milestones ---
internal/dbserver      dbserver message codec (pure) + TCP server (menus/metadata/analysis)
internal/nfs           ONC RPC + XDR codec, portmapper, MOUNT, NFSv2 (read-only, sandboxed)
internal/library       track model + Library interface; folder scanner; rekordbox-USB importer
internal/anlz          ANLZ (.DAT/.EXT/.2EX) reader (later: writer) → beat grid, waveforms, cues
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

- Device type 3 (rekordbox), device number 0x11 (17), as Deep Symmetry observed.
  The name defaults to `Slipmat` and the 0x11 host name defaults to the
  computer name (both configurable).
- Claim sequence and keep-alive cadence follow Vynull for now: 0x00 ×3 pairs,
  0x02 bursts cycling through {17, 18, 41–44}, 0x02 keep-alive plus 0x06 every
  third tick. **Replace this with the cadence seen in our rekordbox 7
  capture.**
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
or mirror port. Run `sudo tcpdump -i <lan-if> -s 0 -w captures/<name>.pcap`.

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

## 9. Licensing

Slipmat is **GPL-3.0-or-later**. That allows studying and adapting code from
Vynull (GPL-3.0) with attribution. Files that adapt Vynull code say so in a
header comment. Deep Symmetry projects (EPL-2.0) are used as documentation
only: we don't copy code, and crate-digger's `.ksy` files are not vendored.
pyrekordbox (MIT), prolink-connect (MIT), python-prodj-link (Apache-2.0) and
rekordcrate (MPL-2.0) are references only.
