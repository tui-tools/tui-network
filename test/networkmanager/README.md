# NetworkManager end-to-end test

`run.sh` drives the NetworkManager backend against a real NetworkManager, in a
Fedora 44 or Ubuntu 24.04 container that runs systemd. The container has a
network namespace of its own, so nothing here touches the host's network.

```sh
make build
test/networkmanager/run.sh fedora
test/networkmanager/run.sh ubuntu
```

`topology.sh` gives NetworkManager three links to manage: `eth1`, a veth with
a static profile; `dummy0`, a second uplink on a dummy device; and `eth2`, a
veth that takes its address over DHCP from `dnsmasq` on its peer. The peers stay
unmanaged.

`e2e.sh` runs as an unprivileged user with `sudo -n`, reads the machine with
`--check` and `--report`, then drives the TUI under `tmux` and checks each
result with `nmcli` and `ip`:

- runtime DNS on `eth1`, kept;
- `eth1` disconnected and rolled back from the dialog;
- `eth1` disconnected and the session killed: NetworkManager restores it by
  itself when the 60-second checkpoint runs out;
- the profile editor writing a new address into `eth1`'s profile;
- a lease renewal on `eth2`;
- `dummy0` made the default route at runtime, then its metric kept in its
  profile;
- a profile reload, which is not wrapped in a checkpoint;
- no checkpoint left behind at the end.

Each screen it acts on is saved under `/tmp/e2e` in the container.
