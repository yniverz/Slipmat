# RPC fixtures: rekordbox 7 and CDJ-3000

These are UDP payloads cut from a capture of rekordbox 7 (macOS) serving two
CDJ-3000s on firmware 3.22. MAC addresses and the computer name have been
anonymised.

| File | What |
|------|------|
| `cdj-50111-76.hex` | CDJ → portmap (UDP 50111): GETPORT for MOUNT v1/UDP |
| `rb-from50111-28.hex` | rekordbox → CDJ: GETPORT reply, port 35008 |
| `cdj-35008-60.hex` | CDJ → mountd: EXPORT |
| `rb-from35008-28.hex` | EXPORT reply while rekordbox is still starting (empty list) |
| `rb-from35008-80.hex` | EXPORT reply when ready: `/` for `192.168.1.103/255.255.255.0` |
| `cdj-35008-68.hex` | CDJ → mountd: MNT `/` |
| `rb-from35008-60.hex` | MNT reply: OK, all-zero 32-byte root handle |
| `rb-from35008-24.hex` | UMNT reply (void) |
