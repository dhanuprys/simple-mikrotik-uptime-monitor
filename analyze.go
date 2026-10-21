package main

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// hbEv is one heartbeat or boot event received from the router.
// T is the server receive time (unix seconds), Up the router uptime in
// seconds as reported by the router (-1 when unknown). Boot is true when
// the event was an explicit boot notification.
type hbEv struct {
	T    int64
	Up   int64
	Boot bool
}

const (
	CausePower    = "power"    // router rebooted during the gap
	CauseInternet = "internet" // router stayed up, but heartbeats did not arrive
	CauseUnknown  = "unknown"  // no uptime info to tell the two apart
	CauseMonitor  = "monitor"  // this monitor server itself was not running
	CauseReboot   = "reboot"   // quick reboot, no heartbeat gap
	CauseBoot     = "boot"     // router boot notification
)

// Segment is a period of time that is not "normal".
type Segment struct {
	S       int64  `json:"s"`
	E       int64  `json:"e"`
	Cause   string `json:"cause"`
	Ongoing bool   `json:"ongoing"`
}

func plausible(t int64) bool { return t > 1_000_000_000 && t < 4_000_000_000 }

// holeAfter is how long the monitor may go without writing anything (it ticks
// every 30s) before we conclude that the monitor itself was not running. It is
// deliberately much smaller than the router's DOWN_AFTER: otherwise a monitor
// restart of, say, 150s could make a healthy router look like it had an outage.
const holeAfter = 70

// parseLog reads log lines. Only complete (newline-terminated) and valid
// lines are used; everything else is silently skipped.
//
//	H,<unix>,<router uptime seconds or -1>   heartbeat from the router
//	B,<unix>,<router uptime seconds or -1>   boot notification from the router
//	T,<unix>                                 "monitor is alive" tick
func parseLog(data []byte, hbs *[]hbEv, alive *[]int64) {
	for {
		i := bytes.IndexByte(data, '\n')
		if i < 0 {
			return // unterminated tail (possibly a partial write) is ignored
		}
		line := string(data[:i])
		data = data[i+1:]
		f := strings.Split(strings.TrimSpace(line), ",")
		switch {
		case len(f) == 3 && (f[0] == "H" || f[0] == "B"):
			t, e1 := strconv.ParseInt(f[1], 10, 64)
			up, e2 := strconv.ParseInt(f[2], 10, 64)
			if e1 != nil || e2 != nil || !plausible(t) {
				continue
			}
			if up < 0 {
				up = -1
			}
			*hbs = append(*hbs, hbEv{T: t, Up: up, Boot: f[0] == "B"})
			*alive = append(*alive, t)
		case len(f) == 2 && f[0] == "T":
			t, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil || !plausible(t) {
				continue
			}
			*alive = append(*alive, t)
		}
	}
}

// parseUptime understands RouterOS uptime strings such as "1w2d03:04:05",
// "3d04:05:06", "04:05:06", "1h2m3s" or plain seconds. Returns -1 if unknown.
func parseUptime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		if n < 0 {
			return -1
		}
		return n
	}
	var total, num int64
	var have bool
	var colons []int64
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
			num = num*10 + int64(c-'0')
			have = true
			if num > 1<<40 {
				return -1
			}
		case c == ':':
			if !have {
				return -1
			}
			colons = append(colons, num)
			num, have = 0, false
		case c == 'w' || c == 'd' || c == 'h' || c == 'm' || c == 's':
			if !have || len(colons) > 0 {
				return -1
			}
			total += num * unitSeconds(c)
			num, have = 0, false
		default:
			return -1
		}
	}
	if len(colons) > 0 {
		if !have || len(colons) != 2 {
			return -1
		}
		total += colons[0]*3600 + colons[1]*60 + num
	} else if have {
		return -1 // trailing number without a unit
	}
	return total
}

func unitSeconds(c rune) int64 {
	switch c {
	case 'w':
		return 7 * 86400
	case 'd':
		return 86400
	case 'h':
		return 3600
	case 'm':
		return 60
	}
	return 1
}

// subtract removes the (sorted, non-overlapping) holes from [s,e].
func subtract(s, e int64, holes [][2]int64) [][2]int64 {
	var out [][2]int64
	cur := s
	for _, h := range holes {
		if h[1] <= cur || h[0] >= e {
			continue
		}
		if h[0] > cur {
			out = append(out, [2]int64{cur, h[0]})
		}
		if h[1] > cur {
			cur = h[1]
		}
	}
	if cur < e {
		out = append(out, [2]int64{cur, e})
	}
	return out
}

// analyze turns raw events into segments.
//
//   - A gap between two heartbeats longer than downAfter is a router outage.
//     If the router's uptime went backwards it rebooted (power), otherwise
//     the router stayed up and the connection was down (internet).
//   - An explicit boot notification is reported as "boot" and confirms
//     that an outage was due to power loss / reboot.
//   - Periods where this monitor was not running (no heartbeat and no tick
//     for longer than holeAfter) are reported as "monitor" and are never
//     blamed on the router.
//   - An uptime reset without a long gap is a quick "reboot" marker.
//
// hbs and alive must be sorted ascending. alive includes heartbeat times.
func analyze(hbs []hbEv, alive []int64, now, downAfter int64) []Segment {
	if len(hbs) == 0 {
		return nil
	}
	first := hbs[0].T

	var holes [][2]int64
	for i := 1; i < len(alive); i++ {
		a, b := alive[i-1], alive[i]
		if b-a > holeAfter && a >= first {
			holes = append(holes, [2]int64{a, b})
		}
	}

	type cand struct {
		s, e    int64
		cause   string
		ongoing bool
	}
	var cands []cand
	var segs []Segment

	lastBootT := int64(-1)
	addBoot := func(t int64) {
		if lastBootT >= 0 && t-lastBootT < 60 {
			return
		}
		lastBootT = t
		segs = append(segs, Segment{S: t - 30, E: t, Cause: CauseBoot})
	}

	if hbs[0].Boot {
		addBoot(hbs[0].T)
	}

	for i := 1; i < len(hbs); i++ {
		a, b := hbs[i-1], hbs[i]
		gap := b.T - a.T
		known := a.Up >= 0 && b.Up >= 0
		// If the router had stayed up, b.Up would be about a.Up+gap.
		rebooted := (known && b.Up+30 < a.Up+gap) || b.Boot
		switch {
		case gap > downAfter:
			cause := CauseUnknown
			if known || b.Boot {
				if rebooted {
					cause = CausePower
				} else {
					cause = CauseInternet
				}
			}
			cands = append(cands, cand{s: a.T, e: b.T, cause: cause})
			if b.Boot {
				addBoot(b.T)
			}
		case b.Boot:
			addBoot(b.T)
		case rebooted:
			segs = append(segs, Segment{S: a.T, E: b.T, Cause: CauseReboot})
		}
	}
	if last := hbs[len(hbs)-1]; now-last.T > downAfter {
		cands = append(cands, cand{s: last.T, e: now, cause: CauseUnknown, ongoing: true})
	}

	for _, c := range cands {
		for _, p := range subtract(c.s, c.e, holes) {
			// Leftovers no longer than the normal heartbeat window are not
			// outages; they only appear next to a monitor-offline hole.
			if p[1]-p[0] <= downAfter {
				continue
			}
			segs = append(segs, Segment{S: p[0], E: p[1], Cause: c.cause, Ongoing: c.ongoing && p[1] == c.e})
		}
	}
	for _, h := range holes {
		segs = append(segs, Segment{S: h[0], E: h[1], Cause: CauseMonitor})
	}
	sort.Slice(segs, func(i, j int) bool {
		if segs[i].S != segs[j].S {
			return segs[i].S < segs[j].S
		}
		return segs[i].E < segs[j].E
	})
	return segs
}

func fmtDur(s int64) string {
	if s < 0 {
		s = 0
	}
	d, h, m, sec := s/86400, s%86400/3600, s%3600/60, s%60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh", d, h)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, sec)
	}
	return fmt.Sprintf("%ds", sec)
}
