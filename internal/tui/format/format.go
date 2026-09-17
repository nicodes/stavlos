// Package format renders numbers, durations, paths and ids the way every
// TUI view shows them.
package format

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// ShortHome abbreviates the home directory prefix as "~".
func ShortHome(p string) string {
	if h, err := os.UserHomeDir(); err == nil && h != "" && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}

// ShortID is the first eight characters of an id.
func ShortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// Tokens renders a token count as 950, 12k or 1.2m.
func Tokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strings.TrimSuffix(strconv.FormatFloat(float64(n)/1_000_000, 'f', 1, 64), ".0") + "m"
	case n >= 1000:
		return strconv.Itoa((n+500)/1000) + "k"
	}
	return strconv.Itoa(n)
}

// Cost renders a dollar amount with two to four decimals.
func Cost(v float64) string {
	s := strconv.FormatFloat(v, 'f', 4, 64)
	dot := strings.IndexByte(s, '.')
	for len(s)-dot-1 > 2 && strings.HasSuffix(s, "0") {
		s = s[:len(s)-1]
	}
	return s
}

// Elapsed renders a duration as 12s, 1m05s or 1h02m.
func Elapsed(d time.Duration) string {
	d = d.Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// months are the short month names Ago dates with ("Sept", not "Sep").
var months = [...]string{"Jan", "Feb", "Mar", "Apr", "May", "June", "July", "Aug", "Sept", "Oct", "Nov", "Dec"}

// Ago renders how long before now at was: "now" under a minute, then
// "5m", "3h" and "2d", and from three days on the date
// ("Sept 1", with the year when it is not now's).
func Ago(at, now time.Time) string {
	d := now.Sub(at)
	switch {
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 72*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	at = at.In(now.Location())
	date := fmt.Sprintf("%s %d", months[at.Month()-1], at.Day())
	if at.Year() != now.Year() {
		date += fmt.Sprintf(" %d", at.Year())
	}
	return date
}

// Trunc cuts s to n runes, marking the cut with "…".
func Trunc(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// FirstLine is the first line of s (trimmed), marked with "…" when more
// lines follow.
func FirstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + "…"
	}
	return s
}
