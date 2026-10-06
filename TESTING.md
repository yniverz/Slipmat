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

### Milestone 3: loading and playback

1. Quit rekordbox, then run
   `bin/slipmat serve -v 2>&1 | tee captures/slipmat-m3.log`
   (it uses the remembered music folder).
2. Optional, in a second terminal: `scripts/capture.sh slipmat-m3`. If
   anything stalls, this shows exactly what the player waited for.
3. SOURCE → Slipmat: note how long it takes until the menu appears (it
   should now be about a second).
4. PLAYLIST → a folder → load a track, then press play. Waveforms and beat
   grid are expected to be missing (milestone 4).
5. Load a second track on the other deck.

**Send back:** whether audio plays and how long loading took, and the log.

### Milestone 4: beat grid and waveforms from rekordbox analysis

Slipmat reads rekordbox's analysis files from:

- rekordbox's own folder (`~/Library/Pioneer/rekordbox/share/PIONEER/USBANLZ`):
  analyse tracks in rekordbox, then quit it;
- any `PIONEER/USBANLZ` folder **copied from a USB export into the music
  folder** (anywhere inside it);
- extra folders given with `-anlz /path` (repeatable).

Files are matched to tracks by file name (Unicode-normalised). A USB
export's full path decides between tracks that share a file name; without
it, such names are skipped with a warning.

1. `bin/slipmat library` lists every track: `A` = analysis found
   (`+3band` = CDJ-3000 3-band waveform data), with the BPM.
2. `bin/slipmat serve -v 2>&1 | tee captures/slipmat-m4.log`
3. Load an analysed track ("house 26"): the waveform overview, the scrolling
   waveform (RGB and 3-band) and the beat grid should appear immediately.
   Check beat sync against the other deck.
4. Load an unanalysed track (another folder): it should still play, with
   the CDJ analysing it itself as before.

**Send back:** what showed up for each, and the log.

### Own waveforms (tracks without rekordbox analysis)

Slipmat generates every waveform format for tracks rekordbox hasn't
analysed: PWAV/PWV2 mono preview, PWV3 blue/white detail, PWV4/PWV5 colour,
PWV6/PWV7 CDJ-3000 3-band. There is no beat grid, BPM or phrase data. Results
are cached in `~/Library/Caches/slipmat/waveforms`. Once rekordbox analyses a
track, its data replaces ours on the next start.

Order per track: rekordbox analysis > (overview only) the preview a CDJ
uploaded > generated waveforms.

Offline checks:

```bash
bin/slipmat waveforms                         # generate the cache now (~0.35 s/track on 4 cores)
bin/slipmat render -track "Deep Inside" -out captures/w.png   # rekordbox (top) vs ours (bottom)
SLIPMAT_CALIBRATE_DIR=~/Music/Slipmat go test ./internal/waveform -run Generator -v  # scores vs rekordbox
SLIPMAT_LOADSIM_MUSIC=~/Music/Slipmat go test ./cmd/slipmat -run LoadSequence -v     # CDJ load rehearsal
```

On the CDJ-3000:

1. `bin/slipmat serve -v 2>&1 | tee captures/slipmat-wave.log`
2. Load an unanalysed track ("remix party" or "schnell"). You should see the
   overview immediately, the scrolling waveform in RGB, blue and 3-band
   (switch the waveform colour in the CDJ's settings), and no beat grid.
3. Compare with an analysed "house 26" track: do the colours and levels
   look similar?

**Send back:** impressions per waveform type, ideally with a photo of an
analysed and an unanalysed track side by side.
