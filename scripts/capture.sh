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
root=$(cd "$(dirname "$0")/.." && pwd)
if [ -z "$iface" ]; then
	# No saved choice yet: let slipmat guess (only wired interface, or the
	# one where Pro DJ Link traffic is heard).
	if [ ! -x "$root/bin/slipmat" ]; then
		(cd "$root" && go build -o bin/slipmat ./cmd/slipmat)
	fi
	iface=$("$root/bin/slipmat" interfaces -guess 2>/dev/null) || true
fi
if [ -z "$iface" ]; then
	echo "usage: $0 NAME INTERFACE   (could not pick one automatically; run 'bin/slipmat interfaces' to list)" >&2
	exit 2
fi

dir=$root/captures
mkdir -p "$dir"
out="$dir/$name-$(date +%Y%m%d-%H%M%S).pcap"
echo "capturing on $iface -> $out (Ctrl-C to stop; sudo will ask for your password)"
# Full packets, everything except mDNS chatter. -Z drops root after opening
# the interface so the file is owned by you.
sudo tcpdump -i "$iface" -s 0 -U -Z "$(id -un)" -w "$out" 'not (udp port 5353)'
