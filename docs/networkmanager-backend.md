# NetworkManager backend

Design notes for the NetworkManager backend of `tui-network` (issue #21). The
systemd-networkd backend is unchanged by it: the new backend sits behind the
same `network.Backend` interface, and the machine decides which one runs.

## Which backend runs

`backend` in the configuration (or `--backend`) takes `auto`, `networkd` or
`networkmanager`. `auto` is the default, and it asks the machine, never assumes:

1. `systemctl is-active` for `NetworkManager.service` and
   `systemd-networkd.service`.
2. If exactly one of them is active, that one runs.
3. If both are active, the one that manages more links runs. NetworkManager's
   count is the devices `nmcli device` reports in a state other than
   `unmanaged` or `connected (externally)`, loopback excluded; networkd's is
   the links `networkctl list` reports `configured`, `configuring` or
   `pending`. A tie keeps systemd-networkd, which is what every release before
   this one did.
4. If neither is active, systemd-networkd runs when `networkctl` is installed
   (the read-only "not running" view it has always had), NetworkManager when
   only `nmcli` is, and the tool fails with an explanation when neither is.

The sentence that explains the choice is part of the answer: `--check` prints
it in a `selection` block, `--report` on a line of its own. The manager that was
not chosen but is running is still named in the header as "also running", and
every link it owns stays read-only, exactly as NetworkManager's links are
read-only under the networkd backend today.

## Reads

Every read is unprivileged and goes through the kit runner, like the networkd
backend's.

| What | Command |
| --- | --- |
| is NetworkManager running | `nmcli -t -f RUNNING general` |
| devices, addresses, gateways, DNS, DHCP lease | `nmcli -t -f GENERAL,IP4,DHCP4,IP6 device show` |
| connection profiles | `nmcli -t -f NAME,UUID,TYPE,DEVICE,FILENAME connection show`, then `nmcli -t -f connection,ipv4,ipv6 connection show uuid <uuid>` per profile |
| routing table, gateway reachability | `ip -j route`, `ip -j route get <gw>` (the networkd backend's parsers) |
| global resolver view | `resolvectl dns`, when systemd-resolved runs |
| log lines about a link | `journalctl -u NetworkManager --grep <link>` |

Terse output is `KEY:value` per line for `device show` and `connection show
<id>` (split at the first colon), and colon-separated columns with `\:`
escapes for the tabular lists. Secrets are never requested: the profile read
names the `connection`, `ipv4` and `ipv6` settings only.

A connection profile is shown where a `.network` file is shown on networkd: the
detail screen lists its `connection`, `ipv4` and `ipv6` settings, and the file
it lives in. On Ubuntu a profile made with `nmcli` is kept in netplan and
rendered to `/run/NetworkManager/system-connections/netplan-NM-<uuid>.nmconnection`;
`nmcli connection modify` writes back through netplan, so the editor works the
same there.

A device is writable when NetworkManager manages it and an active profile
configured it. It is read-only, with the reason on screen, when it is
`unmanaged`, when it is `connected (externally)` (configured by something else
and only observed by NetworkManager), and for the loopback.

## Changes

Every change is an `nmcli` command (or the networkd backend's `ip route`
builder for a device NetworkManager does not own), previewed in full before it
runs.

| Key | networkd | NetworkManager |
| --- | --- | --- |
| `u` | `networkctl up` | `nmcli device connect <dev>` |
| `d` | `networkctl down` | `nmcli device disconnect <dev>` |
| `c` | `networkctl reconfigure` | `nmcli device reapply <dev>` |
| `n` | `networkctl renew` | `nmcli connection up uuid <uuid> ifname <dev>` (re-activation requests a new lease) |
| `r` | `networkctl reload` | `nmcli connection reload` |
| `f` | `resolvectl flush-caches` | the same, when systemd-resolved runs |
| `s` / `S` | `resolvectl dns` / `domain` | `nmcli device modify <dev> ipv4.dns … ipv6.dns …` / `ipv4.dns-search …` (runtime only, like resolvectl) |
| `e` | write a `.network` file | `nmcli connection modify uuid <uuid> ipv4.method … ipv4.addresses … ipv4.gateway … ipv4.dns … ipv4.dns-search …`, then `nmcli connection up uuid <uuid> ifname <dev>`; a wired device with no profile gets `nmcli connection add type ethernet …` |
| `w` `s`/`x` | `ip route replace default …` | `nmcli device modify <dev> ipv4.route-metric <n>` on a managed device, `ip route replace` on another |
| `w` `P` | a `[Route]` drop-in | `nmcli connection modify uuid <uuid> ipv4.route-metric <n>`, then `nmcli device reapply <dev>` |
| `V` / `X` | `.netdev` units | not offered yet |

The editor's preview is a unified diff of the profile settings it changes, in
`setting.key: value` form, the way `nmcli connection show` prints them, so the
dialog still shows before and after.

The editor covers IPv4 (`auto`, `manual` or `disabled`, address, gateway, DNS
servers of either family, search domains) and leaves every other setting of
the profile as it is.

## Not cutting the session

NetworkManager has the safety net the networkd backend has to warn about
instead: checkpoints. A checkpoint snapshots every device and profile, and
NetworkManager itself restores it when its timeout expires, whether or not the
program that asked for it is still alive.

So every change that can drop connectivity (anything the confirm dialog paints
as dangerous) is wrapped:

1. The confirm dialog shows one more command first, and runs it first:

   ```
   busctl call org.freedesktop.NetworkManager /org/freedesktop/NetworkManager \
     org.freedesktop.NetworkManager CheckpointCreate aouu 0 60 6
   ```

   (all devices, 60 seconds, flags `DELETE_NEW_CONNECTIONS |
   DISCONNECT_NEW_DEVICES`).
2. The change runs.
3. A second dialog asks to keep it, counting down. `y` destroys the checkpoint
   (`CheckpointDestroy`), which keeps the change; `n` rolls back now
   (`CheckpointRollback`). Both are previewed like any other command.
4. If nobody answers, because the session the operator was typing over is the
   one the change cut, NetworkManager rolls back by itself when the 60 seconds
   run out.

If a step of the change fails after the checkpoint was taken, the same dialog
opens with the error, so rolling back is one key away. A machine without
`busctl` gets the change without the net, and the confirm dialog says so.

## Privileges

Reads never escalate. Changes go through the configured prefix (`sudo -n`),
because over SSH polkit refuses an unprivileged `nmcli` change anyway. When the
tool runs as a normal user with escalation turned off (`--sudo ""`), every link
is read-only and the reason says why, instead of offering changes that can only
fail.

## `--check`, `--report`, `--demo`

- `--check` reports `"backend": "networkmanager"`, the `selection` block, and
  the `compat` block for the `networkmanager` backend of `tool.json`
  (`nmcli --version`).
- `--report` names the selected backend, why it was selected, and the
  NetworkManager version.
- `--demo=networkmanager` drives an in-memory NetworkManager machine: a wired
  device on DHCP, a wireless one on a static profile, a container bridge
  NetworkManager only observes, and the loopback. Every change, the checkpoint
  and the keep-or-roll-back dialog included, is built and previewed for real.
  `--demo` alone keeps the networkd sample.

## Testing

- Unit tests: the terse parsers against output captured from Fedora 44
  (NetworkManager 1.56) and Ubuntu 24.04 (NetworkManager 1.46), every builder,
  the detection rules as a table, and the app flows (confirm, keep, roll back,
  timeout) against the fake.
- Smoke: `test/smoke.sh` asserts the NetworkManager case on a real machine:
  the backend it selects, the link and managed counts against `nmcli device`,
  the active profile, the read-only loopback, and that `--backend networkd`
  still keeps every link read-only there.
- End to end: `test/networkmanager/run.sh` drives every change through the
  TUI against NetworkManager under systemd in a Fedora or Ubuntu container,
  checkpoint, keep, rollback and the killed session included.
