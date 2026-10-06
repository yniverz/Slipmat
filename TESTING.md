# Testing Slipmat

## Automated tests

```bash
go test ./...
```

Optional: run against Deep Symmetry's CDJ-2000NXS hardware captures (clone
[dysentery](https://github.com/Deep-Symmetry/dysentery) first):

```bash
SLIPMAT_DYSENTERY_CAPTURES=/path/to/dysentery/doc/assets/captures go test ./...
go test ./internal/prolink -run XXX -fuzz FuzzDescribe -fuzztime 30s
```

What's covered:

- **Golden packets**: real CDJ bytes in `internal/prolink/testdata/` are
  decoded field by field. Our rekordbox keep-alive must match beat-link's
  captured template byte for byte.
- **Robustness**: every truncated or corrupted golden packet and the fuzzer's
  inputs must decode to an error, never a panic.
- **pcap**: classic pcap and pcapng, macOS PKTAP, IPv4 fragment reassembly,
  and garbage input.
- **Device logic**: the link handshake (0x10→0x11, 0x05→0x06,
  0x46→0x16/0x47) and the claim sequence, tested against fake sockets.

## Recording captures

rekordbox can run on the same Mac as the capture, so there's no second
machine or mirror port. The helper records on your saved interface into
`captures/`, which is git-ignored because it contains your track names:

```bash
scripts/capture.sh rb7-session
```

Check a capture yourself with `bin/slipmat decode captures/<file>.pcap`, or
add `-changes` to hide repeated status packets.

## Hardware checklists

### Milestone 1: presence on the link

**A. Reference capture of real rekordbox (once, about 3 minutes).** This
gives us ground truth for milestones 1–4 in one go.

1. **Quit rekordbox.** Leave both CDJ-3000s on and connected.
2. `scripts/capture.sh rb7-session`, then enter your password.
3. Start rekordbox 7. Wait until the CDJs show the Mac as linked (about
   20 s).
4. On one CDJ: open the Mac under SOURCE. Browse into a few menus (Track,
   Playlist, and a folder if you have one), and scroll a track list.
5. Load an **MP3** and play it for about 10 s. Then load a **FLAC**, **WAV**
   or **AIFF** and play it for about 10 s.
6. Quit rekordbox with the CDJs still on. Wait 10 s, then press Ctrl-C.
7. Tell me the file name in `captures/`. I can read it directly on this
   machine.

**B. Slipmat on the link.**

1. Quit rekordbox (Slipmat refuses to start while rekordbox holds the
   ports).
2. Build and run:
   ```bash
   go build -o bin/slipmat ./cmd/slipmat
   bin/slipmat serve -v 2>&1 | tee captures/slipmat-m1.log
   ```
   On the first run it detects the interface the CDJs are on, or asks you,
   and remembers the choice.
3. Optional but very helpful: in a second terminal, run
   `scripts/capture.sh slipmat-m1` during the test.
4. On a CDJ-3000, press **SOURCE**. Note whether "Slipmat" appears, and what
   it looks like next to USB/SD. Select it and note what happens. Browsing
   is expected to fail at this milestone, because there's no dbserver yet.
5. Watch the log for `player asked who we are`, `player asked about our
   media` and `link established`.
6. Press Ctrl-C after about 30 s and note how long the CDJ takes to drop
   "Slipmat".

**Send back:**

- `captures/slipmat-m1.log` and the capture file names.
- What the CDJ showed, as a photo or description, and any error messages on
  its screen.
- Your CDJ firmware version (shown under UTILITY → SYSTEM INFO).

### Milestone 2: browsing

1. Quit rekordbox, then run:
   ```bash
   go build -o bin/slipmat ./cmd/slipmat
   bin/slipmat serve -v -music /Users/lennart/Music/Slipmat 2>&1 | tee captures/slipmat-m2.log
   ```
   `-music` is remembered, so later runs need only `bin/slipmat serve -v`.
2. On a CDJ-3000, select Slipmat under SOURCE. The root menu should show
   ARTIST, ALBUM, TRACK and PLAYLIST.
3. Try each menu:
   - **TRACK:** all tracks with titles. Scroll to the end to check paging.
   - **PLAYLIST:** your folders. Folders with sub-folders open as folders,
     the others as playlists of their tracks.
   - **ARTIST / ALBUM:** only populated for tagged files.
4. Highlight a track and open its info popup: duration, BPM and key where
   the tags have them.
5. Loading a track is expected to fail until milestone 3 (NFS).

**Send back:** `captures/slipmat-m2.log`, and what looked wrong or hung
(which menu, which item).
