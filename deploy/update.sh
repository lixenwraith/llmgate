#!/bin/sh
# Updates a running llm jail from this checkout: what make jail built, the rc
# scripts, the app's assets, and llmgate.toml and ipfw.rules rendered again
# with the installed llmgate.toml's listen, proxies and model.name. All is
# checked before anything is installed; what changed is restarted.
#   deploy/update.sh [-j JAIL]     as root on the jail's host; JAIL: llm
set -eu

jail=llm
while getopts j: opt; do
	case $opt in
	j) jail=$OPTARG ;;
	*) echo "usage: $0 [-j JAIL]" >&2; exit 2 ;;
	esac
done

die() {
	echo "${0##*/}: $*" >&2
	exit 1
}

src=$(cd -- "$(dirname -- "$0")/../jail" && pwd)
[ "$(id -u)" -eq 0 ] || die "run as root: it writes into the jail"
for f in llmgate lw; do
	[ -x "$src/usr/local/bin/$f" ] || die "no $src/usr/local/bin/$f: make jail first, as the checkout's owner"
done
root=$(jls -j "$jail" path 2>/dev/null) || die "no running jail $jail"
conf=usr/local/etc/llmgate/llmgate.toml

# The deployment's own values live in the jail alone
addr=$(sed -n 's/^listen = "\(.*\):[0-9]*"$/\1/p' "$root/$conf")
proxy=$(sed -n 's/^proxies = \["\([^"]*\)"\]$/\1/p' "$root/$conf")
model=$(sed -n '/^\[model\]/,/^\[/s/^name = "\(.*\)"$/\1/p' "$root/$conf")
if [ -z "$addr" ] || [ -z "$proxy" ] || [ -z "$model" ]; then
	die "$root/$conf: no listen, proxies or model.name to keep"
fi

tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT
mkdir "$tmp/work" "$tmp/etc"
for f in "$conf" etc/ipfw.rules; do
	sed -e "s|\${JAIL_IP}|$addr|g" -e "s|\${NGINX_ADDR}|$proxy|g" -e "s|\${MODEL}|$model|g" "$src/$f" >"$tmp/etc/${f##*/}"
done

# The new llmgate, lw, assets and configuration, under the jail's own
# llmgate_env, must pass -check before anything is installed
# shellcheck disable=SC2046 # llmgate_env is words, as rc.subr reads it
env $(jexec "$jail" sysrc -n llmgate_env 2>/dev/null || :) \
	LLMGATE_APPS_LWDESCRIBE_LW="$src/usr/local/bin/lw" \
	LLMGATE_APPS_LWDESCRIBE_ASSETS="$src/usr/local/etc/llmgate/lwdescribe" \
	LLMGATE_APPS_LWDESCRIBE_WORK_DIR="$tmp/work" \
	"$src/usr/local/bin/llmgate" -c "$tmp/etc/llmgate.toml" -check

changed=" "
put() { # MODE FROM TO: installs FROM as TO, a path in the jail, when they differ
	cmp -s "$2" "$root/$3" && return
	install -o root -g wheel -m "$1" "$2" "$root/$3"
	changed="$changed$3 "
}
put 0555 "$src/usr/local/bin/llmgate" usr/local/bin/llmgate
put 0555 "$src/usr/local/bin/lw" usr/local/bin/lw
for f in llm llmgate; do
	put 0555 "$src/usr/local/etc/rc.d/$f" "usr/local/etc/rc.d/$f"
done
put 0644 "$src/usr/local/etc/newsyslog.conf.d/llm.conf" usr/local/etc/newsyslog.conf.d/llm.conf
for f in prompt.md examples.toml guide.md; do
	put 0644 "$src/usr/local/etc/llmgate/lwdescribe/$f" "usr/local/etc/llmgate/lwdescribe/$f"
done
put 0644 "$tmp/etc/llmgate.toml" "$conf"
put 0644 "$tmp/etc/ipfw.rules" etc/ipfw.rules

if [ "$changed" = " " ]; then
	echo "$jail: up to date"
	exit 0
fi
echo "$jail: installed$changed"
case $changed in *" etc/ipfw.rules "*) jexec -l "$jail" service ipfw restart ;; esac
case $changed in *" usr/local/etc/rc.d/llm "*) jexec -l "$jail" service llm restart ;; esac
jexec -l "$jail" service llmgate restart

# llmgate warms the model before it opens; its status page says when
admin=$(sed -n 's/^admin = "\(.*\)"$/\1/p' "$tmp/etc/llmgate.toml")
[ -n "$admin" ] || { echo "$jail: llmgate restarted; no admin listener to watch it open"; exit 0; }
for _ in $(seq 300); do
	if jexec "$jail" fetch -qo - "http://$admin/" 2>/dev/null | grep -q '"ready":true'; then
		echo "$jail: llmgate open"
		exit 0
	fi
	sleep 1
done
die "llmgate not open after 300 s: see /var/log/llmgate/llmgate.log in $jail"
