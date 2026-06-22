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
	if command -v "$cmd" >/dev/null 2>&1; then
		echo "$cmd"
	elif command -v nix >/dev/null 2>&1; then
		local store
		store=$(nix build "$nixpkg" --print-out-paths --no-link 2>/dev/null) || true
		if [ -n "${store:-}" ] && [ -x "$store/bin/$cmd" ]; then
			echo "$store/bin/$cmd"
		else
			echo "ERROR: $cmd not found in PATH and nix build failed for $nixpkg" >&2
			echo "Install it: apt install $cmd   (or the equivalent for your OS)" >&2
			exit 1
		fi
	else
		echo "ERROR: $cmd not found in PATH (and nix is not available)" >&2
		echo "Install it: apt install $cmd   (or the equivalent for your OS)" >&2
		exit 1
	fi
}

SWAKS=$(resolve swaks  nixpkgs#swaks)
MUTT=$(resolve  mutt   nixpkgs#mutt)
MSMTP=$(resolve msmtp  nixpkgs#msmtp)
CURL=$(resolve  curl   nixpkgs#curl)
NC=$(resolve    nc     nixpkgs#netcat-gnu)
