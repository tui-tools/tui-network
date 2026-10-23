#!/bin/bash
# End-to-end test of tui-network's NetworkManager backend against a real
# NetworkManager, driven through the TUI with tmux. Runs inside a lab container
# as the unprivileged user "lab" (sudo -n works). Prints PASS/FAIL lines and
# saves a screen capture of every step under $OUT.
set -uo pipefail
BIN=${BIN:-/usr/local/bin/tui-network}
OUT=${OUT:-/tmp/e2e}
mkdir -p "$OUT"
pass=0
fail=0

ok() { printf 'PASS  %s\n' "$1"; pass=$((pass + 1)); }
bad() { printf 'FAIL  %s\n' "$1"; [[ -n ${2:-} ]] && sed 's/^/      | /' <<<"$2" | head -15; fail=$((fail + 1)); }
expect() { # label, actual, pattern
  if grep -qE "$3" <<<"$2"; then ok "$1"; else bad "$1" "$2"; fi
}

checkpoints() {
  busctl get-property org.freedesktop.NetworkManager /org/freedesktop/NetworkManager \
    org.freedesktop.NetworkManager Checkpoints
}

# --- the TUI under tmux ---------------------------------------------------
start() {
  tmux kill-session -t tn 2>/dev/null
  tmux new-session -d -s tn -x 150 -y 45 "TERM=xterm-256color $BIN"
  sleep 3
}
keys() { for k in "$@"; do tmux send-keys -t tn "$k"; sleep 0.7; done; }
text() { tmux send-keys -t tn -l "$1"; sleep 0.5; }
# snap saves the screen under a running number; the number lives in a file
# because snap is often the left side of a pipe, which is a subshell.
snap() {
  local n; n=$(($(cat "$OUT/.n" 2>/dev/null || echo 0) + 1)); echo "$n" >"$OUT/.n"
  local f; f="$OUT/$(printf %02d "$n")-$1.txt"
  tmux capture-pane -p -t tn >"$f"; cat "$f"
}
# pick filters the links screen down to one link (esc would quit there).
pick() { keys / ; tmux send-keys -t tn C-u; text "$1"; keys Enter; }

echo "--- $(. /etc/os-release && echo "$PRETTY_NAME"), $(nmcli --version)"

# 1. Read path, unprivileged.
check=$($BIN --check)
expect "--check selects NetworkManager" "$check" '"backend": "networkmanager"'
expect "--check says why" "$check" '"detail": "NetworkManager is running and systemd-networkd is not"'
expect "--check counts eth1, dummy0 and eth2 as managed" "$check" '"managed": 3'
expect "--check probes the NetworkManager version" "$check" '"version": "1\.[0-9]+'
expect "--check reads the DHCP lease of eth2" "$check" '"Server": "203\.0\.113\.1"'
expect "--check names the profile file" "$check" '"Path": ".*\.nmconnection"'
expect "--check sees three uplinks" "$($BIN --check | python3 -c 'import json,sys;print(json.load(sys.stdin)["gateways"]["count"])')" '^3$'
report=$($BIN --report)
expect "--report names the backend" "$report" '^backend: networkmanager'
expect "--report says why" "$report" '^selected: NetworkManager is running'
noesc=$($BIN --check --sudo "" | python3 -c 'import json,sys;print(json.load(sys.stdin)["managed"])')
expect "with escalation off every link is read-only" "$noesc" '^0$'

# 2. Runtime DNS on eth1: guarded, kept.
start
pick eth1
keys s C-u
text "203.0.113.53"
keys Enter
snap dns-confirm | grep -q CheckpointCreate && ok "the DNS change is previewed with the checkpoint first" || bad "the DNS change is previewed with the checkpoint first"
keys y
sleep 2
snap dns-keep | grep -q "Keep this change" && ok "a keep dialog follows the change" || bad "a keep dialog follows the change"
keys y
sleep 2
expect "eth1 resolves through 203.0.113.53" "$(nmcli -g IP4.DNS device show eth1)" '^203\.0\.113\.53$'
expect "keeping discards the checkpoint" "$(checkpoints)" '^ao 0$'

# 3. Disconnect eth1, roll back from the dialog.
pick eth1
keys d
keys y
sleep 3
expect "eth1 is disconnected after the change" "$(nmcli -g GENERAL.STATE device show eth1)" 'disconnected'
snap down-keep >/dev/null
keys n
sleep 4
expect "n rolls eth1 back to connected" "$(nmcli -g GENERAL.STATE device show eth1)" '^100 \(connected\)'
expect "the rollback consumed the checkpoint" "$(checkpoints)" '^ao 0$'

# 4. Disconnect eth1 and lose the session: NetworkManager rolls back alone.
pick eth1
keys d
keys y
sleep 3
expect "eth1 is down while the dialog waits" "$(nmcli -g GENERAL.STATE device show eth1)" 'disconnected'
tmux kill-session -t tn
echo "      session killed; waiting for NetworkManager's own rollback"
sleep 65
expect "with nobody answering, NetworkManager restored eth1" "$(nmcli -g GENERAL.STATE device show eth1)" '^100 \(connected\)'
expect "and its checkpoint is gone" "$(checkpoints)" '^ao 0$'

# 5. The profile editor on eth1.
start
pick eth1
keys e
snap editor >/dev/null
keys Tab
tmux send-keys -t tn C-u; text "192.0.2.20/24"
keys Tab Tab Tab Enter
snap editor-confirm | grep -q "+ipv4.addresses: 192.0.2.20/24" && ok "the editor diffs the profile settings" || bad "the editor diffs the profile settings"
keys y
sleep 4
keys y
sleep 2
expect "the profile keeps the new address" "$(nmcli -g ipv4.addresses connection show lab-eth1)" '^192\.0\.2\.20/24$'
expect "and eth1 carries it" "$(ip -4 -br addr show eth1)" '192\.0\.2\.20/24'

# 6. Renew the DHCP lease on eth2.
pick eth2
keys n y
sleep 5
snap renew-keep >/dev/null
keys y
sleep 2
expect "eth2 has a DHCP lease after the re-activation" "$(nmcli -g DHCP4.OPTION device show eth2)" 'dhcp_server_identifier = 203\.0\.113\.1'

# 7. Gateways: make dummy0 the default, then keep the priority in its profile.
keys w
sleep 2
snap gateways >/dev/null
keys j s
snap gw-confirm | grep -q "nmcli device modify dummy0 ipv4.route-metric" && ok "the switch is an nmcli runtime change" || bad "the switch is an nmcli runtime change"
keys y
sleep 3
keys y
sleep 2
expect "dummy0 is now the lowest-metric default" "$(ip route show default | head -1)" 'dev dummy0'
keys R
sleep 1
keys k  # the list re-sorts with dummy0 on top; the cursor stays on its row
sleep 2
snap gateways-after >/dev/null
keys P
snap persist-confirm | grep -q "ipv4.route-metric" && ok "persisting diffs the profile metric" || bad "persisting diffs the profile metric"
keys y
sleep 3
keys y
sleep 2
expect "the profile keeps dummy0's metric" "$(nmcli -g ipv4.route-metric connection show lab-uplink2)" '^9[0-9]$'

# 8. Reload is a plain confirm, no checkpoint.
keys Escape r
snap reload-confirm | grep -q CheckpointCreate && bad "reload should not be guarded" || ok "reload is not guarded"
keys y
sleep 1
keys q
tmux kill-session -t tn 2>/dev/null
expect "no checkpoint is left behind" "$(checkpoints)" '^ao 0$'

echo "--- nm-e2e: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
