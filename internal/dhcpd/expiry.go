package dhcpd

import (
	"fmt"
	"time"
)

// renderExpiry turns a lease expiry instant into the short human text the
// screen shows, measured against now: "in 47m", "expired 2m ago", or "now".
// Both servers store the clock differently, so this is the one place the tool
// decides how a lease's time reads.
func renderExpiry(expiry, now time.Time) string {
	d := expiry.Sub(now).Truncate(time.Second)
	switch {
	case d == 0:
		return "now"
	case d > 0:
		return "in " + humanDuration(d)
	default:
		return "expired " + humanDuration(-d) + " ago"
	}
}

// humanDuration renders a duration in the two largest units that apply, so a
// lease reads "1d 3h" rather than "27h0m0s".
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	days := int(d / (24 * time.Hour))
	hours := int((d % (24 * time.Hour)) / time.Hour)
	minutes := int((d % time.Hour) / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}
