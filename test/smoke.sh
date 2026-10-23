#!/bin/bash
# Backend smoke test for tui-network, run inside a lab guest.
#
# The contract (see tui-tools/tui-lab): this script runs on the guest as the
# unprivileged lab user, escalates with `sudo -n` only, prints a short PASS/FAIL
# table and exits non-zero if anything failed. The binary under test is at
# $TUI_LAB_BIN (default: tui-network on PATH).
#
# What it proves is that the tool reads the machine's *real* network and agrees
# with the machine's own tooling — not that a fake renders. The lab already
# covers --version and a --demo frame; this covers the backend.
#
# Two kinds of machine are asserted, because both are normal:
#
#   networkd        Ubuntu cloud images and Omarchy Server: links are managed,
#                   the tool may change them.
#   NetworkManager  Fedora Cloud: the tool selects its NetworkManager backend,
#                   reads the devices and profiles through nmcli, and may
#                   change the devices NetworkManager manages. Forced onto the
#                   networkd backend (--backend networkd) it must still show
#                   every link read-only, as it did before it could drive
#                   NetworkManager.
set -uo pipefail

bin="${TUI_LAB_BIN:-tui-network}"
# TOOL is the manifest name, which is what a compatibility result is keyed on.
TOOL=tui-network
pass=0
fail=0

# check runs one assertion. It takes a label, a command and a grep pattern the
# command's output must match. Output is captured so a failure can show it.
check() {
  local label="$1" command="$2" pattern="$3" output status
  output=$(eval "$command" 2>&1)
  status=$?
  if [[ $status -eq 0 ]] && grep -qE "$pattern" <<<"$output"; then
    printf 'PASS  %s\n' "$label"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s (exit %d)\n' "$label" "$status"
    sed 's/^/      | /' <<<"$output" | head -12
    fail=$((fail + 1))
  fi
}

# check_absent is the inverse of a grep assertion: the command must succeed and
# its output must NOT contain the pattern. It is what proves a link stayed
# read-only, which is a claim about something that did not happen.
check_absent() {
  local label="$1" command="$2" pattern="$3" output status
  output=$(eval "$command" 2>&1)
  status=$?
  if [[ $status -eq 0 ]] && ! grep -qE "$pattern" <<<"$output"; then
    printf 'PASS  %s\n' "$label"
    pass=$((pass + 1))
  else
    printf 'FAIL  %s (exit %d)\n' "$label" "$status"
    sed 's/^/      | /' <<<"$output" | head -12
    fail=$((fail + 1))
  fi
}

# --- compatibility evidence -------------------------------------------------
#
# The manifest's `tested` list is generated, not claimed: it is rebuilt from
# compat/results.jsonl by tui-kit/tools/compat-sync.py, and this is where a
# line of that file comes from. The version recorded is the one the tool itself
# probed, read back out of --check, so it describes the machine that really ran
# the suite rather than what the tester assumed was installed.
#
# The line is printed behind a `compat-result:` prefix so it survives the trip
# out of the guest through the lab's per-VM log, and appended to
# $TUI_COMPAT_RESULTS as well for a run outside the lab.
record_compat() {
  local report="$1" outcome="$2" backend version distro today block
  block=$(sed -n '/"compat": {/,/^  }/p' <<<"$report")
  backend=$(sed -n 's/.*"backend": "\([^"]*\)".*/\1/p' <<<"$block" | head -1)
  version=$(sed -n 's/.*"version": "\([^"]*\)".*/\1/p' <<<"$block" | head -1)
  if [[ -z $backend || -z $version ]]; then
    echo "      no version was probed, so no compatibility result is recorded"
    return
  fi

  distro=$(. /etc/os-release && echo "${ID}-${VERSION_ID:-rolling}")
  today=$(date -u +%Y-%m-%d)
  local line
  line=$(printf '{"backend":"%s","date":"%s","distro":"%s","result":"%s","suite":"smoke","tool":"%s","version":"%s"}' \
    "$backend" "$today" "$distro" "$outcome" "$TOOL" "$version")

  printf 'compat-result: %s\n' "$line"
  if [[ -n ${TUI_COMPAT_RESULTS:-} ]]; then
    printf '%s\n' "$line" >>"$TUI_COMPAT_RESULTS"
  fi
}

echo "--- tui-network smoke on $(. /etc/os-release && echo "$PRETTY_NAME")"

# Which manager this machine really runs, decided the way the tool decides it
# when only one of them is active.
if systemctl is-active --quiet systemd-networkd; then
  manager=networkd
elif systemctl is-active --quiet NetworkManager; then
  manager=networkmanager
else
  manager=none
fi
echo "      manager=$manager"

if [[ $manager != networkmanager ]] && ! command -v networkctl >/dev/null; then
  echo "FAIL  networkctl is not installed on this machine"
  exit 1
fi

backend=systemd-networkd
[[ $manager == networkmanager ]] && backend=networkmanager

# 1. The read path works at all and names the backend it drove. Reading takes
#    no privileges, so this runs as the plain lab user — which is itself the
#    assertion that the tool does not escalate to look.
check "check reads the network unprivileged" \
  "$bin --check" \
  "\"backend\": \"$backend\""

check "check says why it chose $backend" \
  "$bin --check" \
  '"detail": ".+"'

# --- the report block ------------------------------------------------------
#
# --report is read-only and unprivileged like the read path above, so it is
# smoked without sudo. What is asserted is that it agrees with the backend this
# machine drives, that it still answers under --demo, and that it keeps its
# privacy promise — the block goes into a public issue, and everything else
# this tool can print is an interface name or an address, so a home path or the
# host name appearing in it is a bug, not a cosmetic detail.
check "report names the backend it drives" \
  "$bin --report" \
  "^backend: $backend"

check "report says the run was live" \
  "$bin --report" \
  '^mode: live$'

check "report works in demo mode too" \
  "$bin --demo --report" \
  '^backend: demo$'

check "and says so on the mode line" \
  "$bin --demo --report" \
  '^mode: demo'

# The distro and kernel lines are excluded from the host-name search rather
# than from the promise: they are built from /etc/os-release and from uname's
# release and machine fields, never from its nodename, and on a guest called
# "fedora" or "ubuntu" — which is most of them — the host name is a substring
# of the distribution's own. Everything else in the block is searched.
check "report leaks neither a home path nor the host name" \
  "$bin --report | grep -vE '^(distro|kernel): ' | grep -cE '/home/|$(uname -n)' || true" \
  '^0$'

# 2. The link count matches what networkctl lists. This is the real parser
#    test: a tool that fetched the output but failed to parse it reports zero.
if [[ $manager == networkmanager ]]; then
  links=$(nmcli -t -f DEVICE device | grep -c .)
  check "link count matches \`nmcli device\` ($links)" \
    "$bin --check" \
    "\"links\": $links"
else
  links=$(networkctl --no-legend list | grep -cE '^ *[0-9]+ ')
  check "link count matches \`networkctl list\` ($links)" \
    "$bin --check" \
    "\"links\": $links"
fi

# 3. The loopback is always there, and always parsed.
check "the loopback link is parsed" \
  "$bin --check" \
  '"Name": "lo"'

# 4. The routing table is read: a machine the lab can reach has at least one.
routes=$(ip -j route | grep -o '"dst"' | wc -l)
check "route count matches \`ip -j route\` ($routes)" \
  "$bin --check" \
  "\"routes\": $routes"

# 4b. The gateway view agrees with the routing table: every default route that
#     carries a gateway is a candidate uplink. This is the read half of item 7
#     (gateways); switching the default and the failover are driven from the
#     TUI in the router lab, not here.
uplinks=$(ip -j route | python3 -c '
import json, sys
routes = json.load(sys.stdin)
n = 0
for r in routes:
    if r.get("dst") != "default":
        continue
    if r.get("gateway"):
        n += 1
    for nh in r.get("nexthops", []):
        if nh.get("gateway"):
            n += 1
print(n)
' 2>/dev/null || echo skip)
if [[ "$uplinks" == "skip" || -z "$uplinks" ]]; then
  echo "SKIP  could not count uplinks (no python3?)"
else
  check "gateway count matches the default routes ($uplinks)" \
    "$bin --check | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"gateways\"][\"count\"])'" \
    "^$uplinks\$"
fi

case "$manager" in
  networkd)
    # 5. networkd is running, so the tool must say so, and at least one link
    #    must come back managed — otherwise every action would be refused on
    #    a machine where they should all work.
    check "networkd is reported as running" \
      "$bin --check" \
      '"running": true'

    managed=$(networkctl --no-legend list | grep -cE ' (configured|configuring|pending) *$')
    check "at least one link is managed ($managed by networkctl)" \
      "$bin --check" \
      '"managed": [1-9]'

    # 6. A managed link has a .network file, which is what the editor edits.
    check "a .network file was found" \
      "$bin --check" \
      '"configFiles": [1-9]'

    # 6b. The .netdev units are read too, and they agree with the directory.
    #     This is the read half of the VLAN and bridge feature: creating one is
    #     driven from the TUI in the router lab, not here, because it
    #     re-parents a link and would drop this very ssh session. What is
    #     asserted here is that the tool counts the units the machine actually
    #     has — zero on a plain cloud image, which is a real answer, not a
    #     missing read.
    #     The count is over unique file names, in the same way networkd itself
    #     resolves a name that appears in several of its directories.
    netdevs=$(for dir in /etc/systemd/network /run/systemd/network \
      /usr/lib/systemd/network /lib/systemd/network; do
      [[ -d $dir ]] && ls -1 "$dir" 2>/dev/null | grep -E '\.netdev$'
    done | sort -u | wc -l)
    check "the .netdev unit count matches the search path ($netdevs)" \
      "$bin --check | python3 -c 'import json,sys;print(json.load(sys.stdin)[\"netdevs\"])'" \
      "^$netdevs\$"

    # 7. And it is *the right one*. A count alone is not enough: every systemd
    #    ships a handful of world-readable templates in /usr/lib/systemd/network,
    #    so a machine whose real configuration the tool could not open still
    #    reports a non-zero count. This asks networkctl which file configures
    #    the managed link and demands that exact path back — the read the
    #    editor screen opens, asserted read-only.
    #
    #    It is the assertion that caught netplan: on an Ubuntu cloud image the
    #    file is /run/systemd/network/10-netplan-*.network, mode 0640
    #    root:systemd-network, and an unprivileged read of it fails.
    managed_link=$(networkctl --no-legend list |
      awk '$5 ~ /^(configured|configuring|pending)$/ {print $2; exit}')
    netfile=$(networkctl status --no-pager --full "$managed_link" 2>/dev/null |
      sed -n 's/^ *Network File: *//p' | head -1)
    if [[ -z $netfile || $netfile == "n/a" ]]; then
      echo "SKIP  $managed_link has no .network file to match"
    else
      check "the .network file of $managed_link is parsed ($netfile)" \
        "$bin --check" \
        "\"Path\": \"${netfile//./\\.}\""

      # The path can be listed and the contents still be missing, which is
      # exactly how the netplan failure would look after a half fix. Raw is
      # the field the editor renders, so it must not be empty.
      check "its contents were read, not just its name" \
        "$bin --check | grep -A1 '\"Path\": \"$netfile\"'" \
        '"Raw": ".+"'
    fi
    ;;

  networkmanager)
    # 5. NetworkManager is running, so its backend reads the machine and
    #    says so, and the devices it configures come back writable: the
    #    count matches the devices nmcli reports in a state it manages.
    check "NetworkManager is reported as running" \
      "$bin --check" \
      '"running": true'

    nm_managed=$(nmcli -t -f TYPE,STATE device |
      grep -vE '^loopback:|:unmanaged$|externally' | grep -c .)
    check "managed links match nmcli ($nm_managed)" \
      "$bin --check" \
      "\"managed\": $nm_managed"

    # 6. The active profiles are read, and the one behind a connected
    #    device is the one the tool reports for it.
    active_uuid=$(nmcli -t -f UUID,DEVICE connection show --active |
      awk -F: '$2 != "" && $2 != "lo" {print $1; exit}')
    if [[ -n $active_uuid ]]; then
      check "the active profile $active_uuid is read" \
        "$bin --check" \
        "\"ID\": \"$active_uuid\""
    fi

    check "the loopback is read-only with a reason" \
      "$bin --check | python3 -c 'import json,sys;l=[x for x in json.load(sys.stdin)[\"model\"][\"Links\"] if x[\"Name\"]==\"lo\"][0];print(l[\"Managed\"], l[\"ReadOnlyReason\"])'" \
      '^False .+'

    # 7. Forced onto networkd, the old contract holds: networkctl still
    #    lists every link, all of them unmanaged, none writable.
    if command -v networkctl >/dev/null; then
      check "--backend networkd keeps every link read-only" \
        "$bin --backend networkd --check" \
        '"managed": 0'
      check "and names NetworkManager as the reason" \
        "$bin --backend networkd --check" \
        '"ReadOnlyReason": "NetworkManager is running'
    fi
    ;;

  none)
    # Neither manager is running. The links still exist in the kernel, so the
    # read must still work, and nothing may be writable.
    check "the links are read without a manager" \
      "$bin --check" \
      '"running": false'
    check "no link is managed" \
      "$bin --check" \
      '"managed": 0'
    ;;
esac

# 8. --check must never change anything: the link list is identical after it.
snapshot() {
  if [[ $manager == networkmanager ]]; then
    nmcli -t -f DEVICE,STATE,CONNECTION device
  else
    networkctl --no-legend list
  fi
}
before=$(snapshot)
$bin --check >/dev/null 2>&1
after=$(snapshot)
if [[ "$before" == "$after" ]]; then
  printf 'PASS  --check left the network untouched\n'
  pass=$((pass + 1))
else
  printf 'FAIL  --check changed the link list\n'
  diff <(echo "$before") <(echo "$after") | sed 's/^/      | /' | head -12
  fail=$((fail + 1))
fi

if [[ $fail -eq 0 ]]; then
  record_compat "$("$bin" --check 2>/dev/null)" pass
else
  record_compat "$("$bin" --check 2>/dev/null)" fail
fi

echo "--- tui-network: $pass passed, $fail failed"
[[ $fail -eq 0 ]]
