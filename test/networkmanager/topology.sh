#!/bin/bash
# Lab topology inside a NetworkManager container: a static uplink on a veth
# (eth1), a second uplink on a dummy device (dummy0), and a DHCP client link
# (eth2) served by dnsmasq on its veth peer. The peers stay unmanaged.
set -e
ip link add eth1 type veth peer name peer1 2>/dev/null || true
ip link add eth2 type veth peer name peer2 2>/dev/null || true
ip link add dummy0 type dummy 2>/dev/null || true
for p in peer1 peer2; do ip link set $p up; done
ip addr add 192.0.2.1/24 dev peer1 2>/dev/null || true
ip addr add 203.0.113.1/24 dev peer2 2>/dev/null || true
mkdir -p /etc/NetworkManager/conf.d
printf "[keyfile]\nunmanaged-devices=interface-name:peer1;interface-name:peer2\n" > /etc/NetworkManager/conf.d/90-lab.conf
: > /etc/NetworkManager/conf.d/10-globally-managed-devices.conf
systemctl restart NetworkManager
sleep 2
dnsmasq --interface=peer2 --bind-interfaces --port=0 \
  --dhcp-range=203.0.113.100,203.0.113.150,1h \
  --dhcp-option=option:router,203.0.113.1 --dhcp-option=option:dns-server,203.0.113.53 \
  --dhcp-leasefile=/run/lab-dnsmasq.leases --pid-file=/run/lab-dnsmasq.pid
nmcli connection add type ethernet con-name lab-eth1 ifname eth1 ipv4.method manual ipv4.addresses 192.0.2.10/24 ipv4.gateway 192.0.2.1 ipv4.route-metric 100 ipv4.dns 192.0.2.53 ipv4.dns-search example.test ipv6.method disabled >/dev/null
nmcli connection add type dummy con-name lab-uplink2 ifname dummy0 ipv4.method manual ipv4.addresses 198.51.100.10/24 ipv4.gateway 198.51.100.1 ipv4.route-metric 200 ipv6.method disabled >/dev/null
nmcli connection add type ethernet con-name lab-dhcp ifname eth2 ipv4.method auto ipv4.route-metric 300 ipv6.method disabled >/dev/null
nmcli connection up lab-eth1 >/dev/null; nmcli connection up lab-uplink2 >/dev/null; nmcli connection up lab-dhcp >/dev/null
nmcli -t device
ip route
