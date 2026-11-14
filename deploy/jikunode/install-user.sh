#!/bin/sh
# Install a previously downloaded JikuNode binary for the current user.
set -eu
if [ "$#" -ne 1 ] || [ ! -f "$1" ]; then
  echo "usage: install-user.sh /path/to/jikunode" >&2; exit 2
fi
: "${HOME:?HOME must be set}"
config=${XDG_CONFIG_HOME:-"$HOME/.config"}
case "$config" in
  /*) ;;
  *) echo 'XDG_CONFIG_HOME must be an absolute path' >&2; exit 1 ;;
esac
case "$config" in
  *'
'*|*"$(printf '\r')"*) echo 'XDG_CONFIG_HOME must not contain newlines' >&2; exit 1 ;;
esac
# Quote for systemd unit syntax and prevent specifier expansion in the value.
escaped_config=$(printf '%s' "$config" | sed 's/\\/\\\\/g; s/"/\\"/g; s/%/%%/g')
bin="$HOME/.local/bin/jikunode"
unit="$config/systemd/user/jikunode.service"
mkdir -p "$(dirname "$bin")" "$(dirname "$unit")"
install -m 0755 "$1" "$bin"
# systemd expands %h; it does not invoke a shell for ExecStart.
cat > "$unit" <<UNIT
[Unit]
Description=JikuNode user agent
After=network-online.target

[Service]
Environment="XDG_CONFIG_HOME=$escaped_config"
ExecStart=%h/.local/bin/jikunode run
Restart=always
RestartSec=5
UMask=0077

[Install]
WantedBy=default.target
UNIT
if ! command -v systemctl >/dev/null 2>&1 || ! systemctl --user daemon-reload; then
  echo 'User systemd manager unavailable; start it, then run: systemctl --user enable --now jikunode.service' >&2
  exit 1
fi
if ! systemctl --user enable --now jikunode.service; then
  echo 'Could not start JikuNode; inspect: systemctl --user status jikunode.service' >&2
  exit 1
fi
if ! systemctl --user is-active --quiet jikunode.service; then
  echo 'JikuNode service is not running; inspect: systemctl --user status jikunode.service' >&2
  exit 1
fi
echo 'JikuNode user service enabled and started. For operation after logout, ask an administrator about: loginctl enable-linger USER'
