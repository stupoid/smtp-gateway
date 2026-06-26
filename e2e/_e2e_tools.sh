#!/usr/bin/env bash
# Shared tool resolution for e2e scripts.
# Source this file, then use $SWAKS, $MUTT, $MSMTP, $CURL, $NC.
#
# On NixOS the tools may not be in PATH; the script tries nix as a fallback.
# On Debian/Ubuntu:  apt install swaks mutt msmtp curl netcat-openbsd
# On macOS:          brew install swaks mutt msmtp curl netcat

set -euo pipefail

resolve() {
	local cmd="$1" nixpkg="$2"
	local p
	p=$(command -v "$cmd" 2>/dev/null) || true
	if [ -n "${p:-}" ]; then
		echo "$p"
		return 0
	fi
	if command -v nix >/dev/null 2>&1; then
		local store
		store=$(nix build "$nixpkg" --print-out-paths --no-link 2>/dev/null) || true
		if [ -n "${store:-}" ] && [ -x "$store/bin/$cmd" ]; then
			echo "$store/bin/$cmd"
			return 0
		fi
	fi
	echo "ERROR: $cmd not found (PATH or nix) — install: apt install $cmd" >&2
	exit 1
}

SWAKS=$(resolve swaks  nixpkgs#swaks)
MUTT=$(resolve  mutt   nixpkgs#mutt)
MSMTP=$(resolve msmtp  nixpkgs#msmtp)
CURL=$(resolve  curl   nixpkgs#curl)
NC=$(resolve    nc     nixpkgs#netcat-gnu)
