# Slipmat

Slipmat turns a computer into a **Pro DJ Link media source**, playing the same
role as rekordbox in "Link Export" mode. Point it at a folder of music, and
Pioneer/AlphaTheta players (primary target: CDJ-3000) can find it under
**SOURCE**, browse it, and load and play tracks from it. rekordbox is not
needed at runtime.

> **Status: early development.** Pro DJ Link is an undocumented,
> reverse-engineered protocol. Don't rely on this at a gig.

See [DESIGN.md](DESIGN.md) for architecture, protocols and milestones.

## Requirements

- Go 1.24+ (to build)
- A wired network interface on the same link as the players (DHCP or link-local 169.254.x.x)
- rekordbox must **not** be running on the same machine (both need the same ports)

## Usage

```bash
go build -o bin/slipmat ./cmd/slipmat

bin/slipmat interfaces            # list network interfaces, show which see Pro DJ Link traffic
bin/slipmat monitor               # log all Pro DJ Link traffic (passive, sends nothing)
bin/slipmat decode capture.pcap   # decode a tcpdump/Wireshark capture offline
bin/slipmat serve -music ~/Music  # serve a folder (remembered) as a rekordbox source
bin/slipmat library               # show tracks and which have rekordbox analysis
scripts/capture.sh NAME           # record a tcpdump capture into captures/ (see TESTING.md)
```

Useful flags: `-i/--interface`, `-v` (debug), `-vv` (trace and hex dumps).

## Acknowledgements

This project builds on public reverse-engineering work, above all Deep
Symmetry's [dysentery / DJ Link packet analysis](https://djl-analysis.deepsymmetry.org/),
[beat-link](https://github.com/Deep-Symmetry/beat-link) and
[crate-digger](https://github.com/Deep-Symmetry/crate-digger), and
[Vynull](https://github.com/vynulldev/vynull) (GPL-3.0), whose server-side
findings are adapted here with attribution. Thanks also to python-prodj-link,
prolink-connect, rekordcrate and pyrekordbox.

Not affiliated with Pioneer DJ / AlphaTheta.

## License

GPL-3.0-or-later. See [LICENSE](LICENSE).
