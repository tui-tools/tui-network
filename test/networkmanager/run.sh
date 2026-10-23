#!/bin/bash
# Run the NetworkManager end-to-end test in a container.
#
#   test/networkmanager/run.sh fedora|ubuntu [path/to/tui-network]
#
# The container runs systemd and NetworkManager in a network namespace of its
# own (--network none), so nothing it does reaches the host's network. It
# needs podman and a privileged container, for veth and dummy devices.
set -euo pipefail
distro="${1:?usage: run.sh fedora|ubuntu [binary]}"
here="$(cd "$(dirname "$0")" && pwd)"
bin="${2:-$here/../../bin/tui-network}"
image="tui-network-nm-$distro"
name="tui-network-nm-$distro"

podman build -q -t "$image" -f "$here/Containerfile.$distro" "$here" >/dev/null
podman rm -f "$name" >/dev/null 2>&1 || true
podman run -d --name "$name" --network none --privileged --systemd=always "$image" >/dev/null
trap 'podman rm -f "$name" >/dev/null 2>&1 || true' EXIT
sleep 8

podman cp "$here/topology.sh" "$name:/usr/local/bin/topology.sh"
podman cp "$here/e2e.sh" "$name:/usr/local/bin/e2e.sh"
podman cp "$bin" "$name:/usr/local/bin/tui-network"
podman exec "$name" /usr/local/bin/topology.sh >/dev/null
podman exec -u lab -w /home/lab "$name" /usr/local/bin/e2e.sh
