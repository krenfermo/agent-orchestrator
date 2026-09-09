#!/usr/bin/env bash
# Write the OIDC client secret into a 0600 file, without it passing through
# argv, the shell history or the terminal.
#
# argv is visible in `ps`, and the shell history file survives the session, so
# the secret is read from STDIN and never echoed. Nothing here prints the value:
# the confirmation is the byte length and the file mode, which is enough to tell
# a successful paste from an empty one without revealing anything.
#
#   ./scripts/ao-set-oidc-secret.sh                 # prompts, reads one line
#   pbpaste | ./scripts/ao-set-oidc-secret.sh       # straight from the clipboard
#
# Then export AO_OIDC_CLIENT_SECRET_FILE (see docs/sso-oidc.md) and restart the
# daemon. This script does not restart anything and does not touch the daemon.

set -euo pipefail

target="${AO_OIDC_CLIENT_SECRET_FILE:-$HOME/.ao/oidc-client-secret}"

umask 077
mkdir -p "$(dirname "$target")"

if [ -t 0 ]; then
	# -s keeps it off the screen; it never reaches the history because it is
	# read into a variable rather than typed as an argument.
	printf 'Paste the OIDC client secret (input hidden), then press Enter: ' >&2
	IFS= read -rs secret
	printf '\n' >&2
else
	IFS= read -r secret || true
fi

# Trim surrounding whitespace and any trailing newline the source added. AO
# trims the same way when it reads the file.
secret="$(printf '%s' "$secret" | tr -d '\r\n')"

if [ -z "$secret" ]; then
	printf 'error: nothing was read; the file was not written or changed.\n' >&2
	exit 1
fi

tmp="$(mktemp "${target}.XXXXXX")"
chmod 600 "$tmp"
printf '%s' "$secret" > "$tmp"
mv -f "$tmp" "$target"
chmod 600 "$target"

mode="$(stat -f '%Lp' "$target" 2>/dev/null || stat -c '%a' "$target")"
bytes="$(wc -c < "$target" | tr -d ' ')"

printf 'wrote %s\n' "$target" >&2
printf '  mode:  %s (must be 600)\n' "$mode" >&2
printf '  bytes: %s (compare with the length shown in the provider console)\n' "$bytes" >&2

if [ "$mode" != "600" ]; then
	printf 'error: mode is %s, not 600; AO will refuse to read it.\n' "$mode" >&2
	exit 1
fi
