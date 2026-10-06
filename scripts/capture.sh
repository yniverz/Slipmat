#!/bin/sh
# Record Pro DJ Link traffic for Slipmat development.
#
#   scripts/capture.sh NAME [INTERFACE]
#
# Writes captures/NAME-YYYYmmdd-HHMMSS.pcap (git-ignored). INTERFACE defaults
# to the one saved by `slipmat serve/monitor` (see `slipmat interfaces`).
# rekordbox may run on this same machine: its traffic leaves through the
# interface, so tcpdump sees both directions. Stop with Ctrl-C.
set -eu

name=${1:-capture}
iface=${2:-}
if [ -z "$iface" ]; then
	cfg="$HOME/Library/Application Support/slipmat/config.json"
	[ -f "$cfg" ] || cfg="${XDG_CONFIG_HOME:-$HOME/.config}/slipmat/config.json"
	if [ -f "$cfg" ]; then
		iface=$(sed -n 's/.*"interface": *"\([^"]*\)".*/\1/p' "$cfg")
	fi
fi
if [ -z "$iface" ]; then
	echo "usage: $0 NAME INTERFACE   (no saved interface; run 'slipmat interfaces' to list)" >&2
	exit 2
fi

dir=$(cd "$(dirname "$0")/.." && pwd)/captures
mkdir -p "$dir"
out="$dir/$name-$(date +%Y%m%d-%H%M%S).pcap"
echo "capturing on $iface -> $out (Ctrl-C to stop; sudo will ask for your password)"
# Full packets, everything except mDNS chatter. -Z drops root after opening
# the interface so the file is owned by you.
sudo tcpdump -i "$iface" -s 0 -U -Z "$(id -un)" -w "$out" 'not (udp port 5353)'
