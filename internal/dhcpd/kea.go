package dhcpd

import (
	"encoding/csv"
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/tui-tools/tui-network/internal/dhcp"
)

// keaConfig is the part of a Kea DHCPv4 configuration tui-network reads. Kea
// wraps everything in a top-level "Dhcp4" (or "Dhcp6") object; only the fields
// the screen shows are decoded, so a config with more in it still reads.
type keaConfig struct {
	Dhcp4 *keaService `json:"Dhcp4"`
	Dhcp6 *keaService `json:"Dhcp6"`
}

// keaService is one address family's configuration.
type keaService struct {
	ValidLifetime int         `json:"valid-lifetime"`
	Subnets       []keaSubnet `json:"subnet4"`
	Subnets6      []keaSubnet `json:"subnet6"`
	Reservations  []keaHost   `json:"reservations"`
}

// keaSubnet is one subnet with its pools and its host reservations.
type keaSubnet struct {
	Subnet        string    `json:"subnet"`
	ValidLifetime int       `json:"valid-lifetime"`
	Pools         []keaPool `json:"pools"`
	Reservations  []keaHost `json:"reservations"`
}

// keaPool is one address pool. Kea writes the range as "start - end", and also
// accepts a prefix form, which is kept verbatim when it is not a range.
type keaPool struct {
	Pool string `json:"pool"`
}

// keaHost is one host reservation.
type keaHost struct {
	HWAddress string `json:"hw-address"`
	ClientID  string `json:"client-id"`
	IPAddress string `json:"ip-address"`
	Hostname  string `json:"hostname"`
}

// ParseKeaConfig reads a Kea DHCP configuration into the pools and reservations
// it declares. Kea's parser accepts // and /* */ comments that strict JSON does
// not, so those are stripped first; a file that still will not parse yields
// nothing rather than an error, because a DHCP screen that cannot read the
// config is not a reason to fail the whole tool.
func ParseKeaConfig(path, raw string) (pools []dhcp.Pool, reservations []dhcp.Reservation) {
	var cfg keaConfig
	if err := json.Unmarshal([]byte(stripJSONComments(raw)), &cfg); err != nil {
		return nil, nil
	}
	service, family := cfg.Dhcp4, "ipv4"
	if service == nil {
		service, family = cfg.Dhcp6, "ipv6"
	}
	if service == nil {
		return nil, nil
	}

	defaultLease := renderKeaLease(service.ValidLifetime)
	subnets := service.Subnets
	if family == "ipv6" {
		subnets = service.Subnets6
	}
	// A reservation can live at the service level (global) or inside a subnet.
	reservations = appendKeaHosts(reservations, path, family, service.Reservations)
	for _, subnet := range subnets {
		lease := defaultLease
		if subnet.ValidLifetime > 0 {
			lease = renderKeaLease(subnet.ValidLifetime)
		}
		for _, pool := range subnet.Pools {
			if p, ok := parseKeaPool(path, family, subnet.Subnet, lease, pool.Pool); ok {
				pools = append(pools, p)
			}
		}
		reservations = appendKeaHosts(reservations, path, family, subnet.Reservations)
	}
	return pools, reservations
}

// parseKeaPool reads one Kea pool string into a pool. Kea writes a pool either
// as a range ("192.0.2.50 - 192.0.2.150") or as a prefix ("192.0.2.0/24"); both
// are accepted, and anything that is neither is rejected rather than shown as a
// pool with a start that is not an address.
func parseKeaPool(path, family, subnet, lease, pool string) (dhcp.Pool, bool) {
	pool = strings.TrimSpace(pool)
	if pool == "" {
		return dhcp.Pool{}, false
	}
	p := dhcp.Pool{Name: subnet, Family: family, LeaseTime: lease, Source: path}
	if start, end, found := strings.Cut(pool, "-"); found {
		s, okStart := parseHost(strings.TrimSpace(start))
		e, okEnd := parseHost(strings.TrimSpace(end))
		if !okStart || !okEnd {
			return dhcp.Pool{}, false
		}
		p.Start, p.End = s, e
		return p, true
	}
	// A single token: a bare address, or a CIDR prefix, and nothing else.
	if addr, ok := parseHost(pool); ok {
		p.Start = addr
		return p, true
	}
	if prefix, err := netip.ParsePrefix(pool); err == nil {
		p.Start = prefix.String()
		return p, true
	}
	return dhcp.Pool{}, false
}

// appendKeaHosts folds a list of Kea host reservations into the model.
func appendKeaHosts(into []dhcp.Reservation, path, family string, hosts []keaHost) []dhcp.Reservation {
	for _, host := range hosts {
		if host.HWAddress == "" && host.ClientID == "" && host.IPAddress == "" {
			continue
		}
		into = append(into, dhcp.Reservation{
			MAC:      host.HWAddress,
			ClientID: host.ClientID,
			IP:       host.IPAddress,
			Hostname: host.Hostname,
			Family:   family,
			Source:   path,
		})
	}
	return into
}

// renderKeaLease renders a Kea valid-lifetime (seconds) as a lease time, empty
// when it was not set.
func renderKeaLease(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	return (time.Duration(seconds) * time.Second).String()
}

// ParseKeaLeasesCSV reads a Kea memfile lease CSV (kea-leases4.csv). The header
// row names the columns, which is read rather than assumed because Kea has
// added columns across versions; the columns this tool shows are looked up by
// name, and a file missing one of them still yields the rest. expire is a Unix
// timestamp, rendered against now so the reading is testable.
func ParseKeaLeasesCSV(raw string, now time.Time) []dhcp.Lease {
	reader := csv.NewReader(strings.NewReader(raw))
	reader.FieldsPerRecord = -1
	records, err := reader.ReadAll()
	if err != nil || len(records) < 2 {
		return nil
	}
	col := map[string]int{}
	for i, name := range records[0] {
		col[strings.TrimSpace(name)] = i
	}
	// Without the address column there is nothing worth showing.
	addrCol, ok := col["address"]
	if !ok {
		return nil
	}

	var leases []dhcp.Lease
	for _, record := range records[1:] {
		get := func(name string) string {
			if i, ok := col[name]; ok && i < len(record) {
				return strings.TrimSpace(record[i])
			}
			return ""
		}
		if addrCol >= len(record) {
			continue
		}
		ip, ok := parseHost(record[addrCol])
		if !ok {
			continue
		}
		// A Kea lease with state 2 (expired-reclaimed) is not a live lease.
		if get("state") == "2" {
			continue
		}
		lease := dhcp.Lease{
			IP:       ip,
			MAC:      get("hwaddr"),
			ClientID: get("client_id"),
			Hostname: get("hostname"),
			Family:   familyOf(ip),
			Expiry:   renderKeaExpiry(get("expire"), now),
		}
		leases = append(leases, lease)
	}
	return leases
}

// renderKeaExpiry renders a Kea "expire" column (Unix seconds) against now.
func renderKeaExpiry(field string, now time.Time) string {
	epoch, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		return ""
	}
	return renderExpiry(time.Unix(epoch, 0), now)
}

// stripJSONComments removes the // line and /* */ block comments Kea allows but
// encoding/json does not. It walks the text so a "//" inside a JSON string is
// left alone; anything else Kea's superset allows (it is otherwise standard
// JSON) is left for the decoder to reject.
func stripJSONComments(raw string) string {
	var b strings.Builder
	inString, escaped := false, false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		if inString {
			b.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			b.WriteByte(c)
		case c == '/' && i+1 < len(raw) && raw[i+1] == '/':
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			if i < len(raw) {
				b.WriteByte('\n')
			}
		case c == '/' && i+1 < len(raw) && raw[i+1] == '*':
			i += 2
			for i+1 < len(raw) && (raw[i] != '*' || raw[i+1] != '/') {
				i++
			}
			i++
		case c == '#':
			for i < len(raw) && raw[i] != '\n' {
				i++
			}
			if i < len(raw) {
				b.WriteByte('\n')
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}
