#!/bin/sh
# Brings this checkout and the LogWisp one (LOGWISP, the one beside this by
# default) to their latest, and builds what deploy/update.sh installs. As the
# checkouts' owner, never root, so what it writes stays theirs. lw is stamped
# with its commit's time: the same commit builds the same lw.
#   [LOGWISP=DIR] deploy/build.sh
set -eu

die() {
	echo "${0##*/}: $*" >&2
	exit 1
}

here=$(cd -- "$(dirname -- "$0")/.." && pwd)
[ "$(id -u)" -ne 0 ] || die "run as the checkouts' owner, then deploy/update.sh as root"
logwisp=$(cd -- "${LOGWISP:-$here/../logwisp}" && pwd) || die "no LogWisp checkout: LOGWISP=DIR"
for repo in "$here" "$logwisp"; do
	git -C "$repo" pull -q --ff-only || die "$repo: git pull --ff-only failed"
done
SOURCE_DATE_EPOCH=$(git -C "$logwisp" log -1 --format=%ct) make -C "$here" jail LOGWISP="$logwisp"
echo "built llmgate $(git -C "$here" rev-parse --short HEAD), lw $(git -C "$logwisp" rev-parse --short HEAD)"
