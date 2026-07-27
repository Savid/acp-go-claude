#!/bin/bash
# Bring up a session bus and a live Secret Service, then idle. The unlock
# password is written with a trailing newline on purpose: gnome-keyring-daemon
# fed a bare end of input claims the org.freedesktop.secrets bus name and never
# creates its collection, which looks alive and answers no lookup.
set -euo pipefail

eval "$(dbus-launch --sh-syntax)"
export DBUS_SESSION_BUS_ADDRESS DBUS_SESSION_BUS_PID

printf 'canary-unlock-password\n' | gnome-keyring-daemon --unlock --daemonize >/dev/null

printf '%s\n' "$DBUS_SESSION_BUS_ADDRESS" > /home/canary/dbus-address

exec sleep infinity
