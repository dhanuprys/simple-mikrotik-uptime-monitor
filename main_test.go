package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const base = int64(1_800_000_000)

// beats builds regular heartbeats every 60s starting at t0 with uptime up0.
func beats(t0, up0 int64, n int) []hbEv {
	var out []hbEv
	for i := 0; i < n; i++ {
		out = append(out, hbEv{T: t0 + int64(i)*60, Up: up0 + int64(i)*60})
	}
	return out
}

// ticks returns monitor liveness ticks every 30s in [from,to], like the real
// server writes while it is running.
func ticks(from, to int64) []int64 {
	var out []int64
	for t := from; t <= to; t += 30 {
		out = append(out, t)
	}
	return out
}

func aliveOf(hbs []hbEv, extra ...int64) []int64 {
	var a []int64
	for _, h := range hbs {
		a = append(a, h.T)
	}
	a = append(a, extra...)
	// simple insertion sort, inputs are tiny
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
	return a
}

func TestParseUptime(t *testing.T) {
	cases := map[string]int64{
		"1w2d03:04:05": 604800 + 2*86400 + 3*3600 + 4*60 + 5,
		"3d04:05:06":   3*86400 + 4*3600 + 5*60 + 6,
		"04:05:06":     4*3600 + 5*60 + 6,
		"00:00:00":     0,
		"1h2m3s":       3723,
		"45s":          45,
		"1234":         1234,
		"":             -1,
		"abc":          -1,
		"1w2d":         9 * 86400,
		"12:34":        -1,
		"5":            5,
		"-5":           -1,
		"1d 03:04:05":  -1,
	}
	for in, want := range cases {
		if got := parseUptime(in); got != want {
			t.Errorf("parseUptime(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestAnalyzeNormalHasNoSegments(t *testing.T) {
	hbs := beats(base, 5000, 30)
	segs := analyze(hbs, aliveOf(hbs), hbs[len(hbs)-1].T+10, 180)
	if len(segs) != 0 {
		t.Fatalf("expected none, got %+v", segs)
	}
}

func TestAnalyzeInternetDown(t *testing.T) {
	hbs := beats(base, 5000, 5)
	last := hbs[len(hbs)-1]
	// 10 minutes without heartbeats, router stayed up
	resume := hbEv{T: last.T + 600, Up: last.Up + 600}
	hbs = append(hbs, resume)
	segs := analyze(hbs, aliveOf(hbs, ticks(last.T, resume.T)...), resume.T+5, 180)
	if len(segs) != 1 || segs[0].Cause != CauseInternet || segs[0].S != last.T || segs[0].E != resume.T {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestAnalyzePowerLoss(t *testing.T) {
	hbs := beats(base, 90000, 5)
	last := hbs[len(hbs)-1]
	resume := hbEv{T: last.T + 900, Up: 40} // router booted 40s ago
	hbs = append(hbs, resume)
	segs := analyze(hbs, aliveOf(hbs, ticks(last.T, resume.T)...), resume.T+5, 180)
	if len(segs) != 1 || segs[0].Cause != CausePower {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestAnalyzePowerLossEvenWhenRouterWasFreshlyBooted(t *testing.T) {
	hbs := beats(base, 20, 3) // router had only been up for ~2 minutes
	last := hbs[len(hbs)-1]
	resume := hbEv{T: last.T + 1200, Up: 30}
	hbs = append(hbs, resume)
	segs := analyze(hbs, aliveOf(hbs, ticks(last.T, resume.T)...), resume.T+5, 180)
	if len(segs) != 1 || segs[0].Cause != CausePower {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestAnalyzeUnknownCause(t *testing.T) {
	hbs := beats(base, 5000, 3)
	last := hbs[len(hbs)-1]
	hbs = append(hbs, hbEv{T: last.T + 600, Up: -1})
	segs := analyze(hbs, aliveOf(hbs, ticks(last.T, last.T+600)...), last.T+700, 180)
	if len(segs) != 1 || segs[0].Cause != CauseUnknown {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestAnalyzeQuickReboot(t *testing.T) {
	hbs := beats(base, 50000, 3)
	last := hbs[len(hbs)-1]
	hbs = append(hbs, hbEv{T: last.T + 60, Up: 25}) // no gap, but uptime reset
	segs := analyze(hbs, aliveOf(hbs), last.T+70, 180)
	if len(segs) != 1 || segs[0].Cause != CauseReboot {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestAnalyzeOngoing(t *testing.T) {
	hbs := beats(base, 5000, 3)
	last := hbs[len(hbs)-1]
	now := last.T + 1000
	segs := analyze(hbs, aliveOf(hbs, ticks(last.T, now)...), now, 180)
	if len(segs) != 1 || !segs[0].Ongoing || segs[0].S != last.T || segs[0].E != now {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestAnalyzeJitterIsNotAnOutage(t *testing.T) {
	hbs := []hbEv{
		{T: base, Up: 1000},
		{T: base + 60, Up: 1060},
		{T: base + 150, Up: 1150},
		{T: base + 200, Up: 1200},
		{T: base + 260, Up: 1260},
	}
	segs := analyze(hbs, aliveOf(hbs, ticks(base, base+260)...), base+270, 180)
	if len(segs) != 0 {
		t.Fatalf("expected none, got %+v", segs)
	}
}

// Monitor restarts while the router is perfectly healthy: only a "monitor"
// segment may appear, never a router outage.
func TestAnalyzeMonitorRestartIsNotRouterOutage(t *testing.T) {
	hbs := beats(base, 5000, 2) // base, base+60
	// monitor dies at base+90 (last tick), is back at base+500
	resume := hbEv{T: base + 520, Up: 5000 + 520}
	hbs = append(hbs, resume)
	alive := aliveOf(hbs, append(ticks(base+60, base+90), ticks(base+500, base+520)...)...)
	segs := analyze(hbs, alive, resume.T+5, 180)
	if len(segs) != 1 || segs[0].Cause != CauseMonitor || segs[0].S != base+90 || segs[0].E != base+500 {
		t.Fatalf("unexpected: %+v", segs)
	}
}

// Regression: the monitor is down for 160s (shorter than DOWN_AFTER=180, longer
// than its 30s tick). The healthy router's beats are lost meanwhile, so the
// heartbeat gap is 240s > DOWN_AFTER. That must NOT be blamed on the router.
func TestAnalyzeShortMonitorRestartDoesNotBlameRouter(t *testing.T) {
	hbs := []hbEv{
		{T: base, Up: 5000},
		{T: base + 60, Up: 5060},
		{T: base + 300, Up: 5300},
		{T: base + 360, Up: 5360},
	}
	alive := aliveOf(hbs, append(ticks(base+60, base+90), ticks(base+250, base+360)...)...)
	segs := analyze(hbs, alive, base+365, 180)
	if len(segs) != 1 || segs[0].Cause != CauseMonitor || segs[0].S != base+90 || segs[0].E != base+250 {
		t.Fatalf("unexpected: %+v", segs)
	}
}

// Router dies, monitor also restarts in the middle: the long leftover is still
// reported against the router, the short leftover is dropped.
func TestAnalyzeRouterOutageWithMonitorHole(t *testing.T) {
	hbs := beats(base, 5000, 2)
	resume := hbEv{T: base + 1500, Up: 30}
	hbs = append(hbs, resume)
	alive := aliveOf(hbs, append(ticks(base+60, base+90), ticks(base+500, base+1500)...)...)
	segs := analyze(hbs, alive, resume.T+5, 180)
	var power, monitor int
	for _, s := range segs {
		switch s.Cause {
		case CausePower:
			power++
			if s.S != base+500 || s.E != base+1500 {
				t.Errorf("power piece wrong: %+v", s)
			}
		case CauseMonitor:
			monitor++
		}
	}
	if power != 1 || monitor != 1 || len(segs) != 2 {
		t.Fatalf("unexpected: %+v", segs)
	}
}

func TestWeekStart(t *testing.T) {
	loc, _ := time.LoadLocation("Asia/Makassar")
	st := &Store{loc: loc}
	got, ok := st.weekStart("2026-W43")
	if !ok || got.Format("2006-01-02 15:04") != "2026-10-19 00:00" {
		t.Fatalf("W43: %v %v", got, ok)
	}
	if got, ok := st.weekStart("2026-W53"); !ok || got.Format("2006-01-02") != "2026-12-28" {
		t.Fatalf("2026 has 53 ISO weeks: %v %v", got, ok)
	}
	for _, bad := range []string{"2025-W53", "2026-W00", "2026-W54", "abc", "2026-43", "1999-W01", ""} {
		if _, ok := st.weekStart(bad); ok {
			t.Errorf("%q should be invalid", bad)
		}
	}
	if got, ok := st.weekStart("2026-W01"); !ok || got.Format("2006-01-02") != "2025-12-29" {
		t.Fatalf("W01: %v %v", got, ok)
	}
	// key and start must round-trip for a full range of weeks
	for y := 2024; y <= 2030; y++ {
		for w := 1; w <= 52; w++ {
			k := fmt.Sprintf("%04d-W%02d", y, w)
			s, ok := st.weekStart(k)
			if !ok || st.weekKey(s) != k || s.Weekday() != time.Monday {
				t.Fatalf("round trip failed for %s: %v %v", k, s, ok)
			}
		}
	}
}

func TestStoreRepairsTruncatedTail(t *testing.T) {
	dir := t.TempDir()
	loc := time.UTC
	now := time.Date(2026, 10, 21, 4, 0, 0, 0, loc)

	st, err := NewStore(dir, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(now, "H,1792000000,100\n"); err != nil {
		t.Fatal(err)
	}
	st.Close()

	path := filepath.Join(dir, st.weekKey(now)+".log")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("H,17920000") // simulated power loss mid-write
	f.Close()

	st2, _ := NewStore(dir, loc)
	if err := st2.Append(now, "H,1792000060,160\n"); err != nil {
		t.Fatal(err)
	}
	start, _ := st2.weekStart(st2.weekKey(now))
	hbs, alive := st2.Load(start)
	if len(hbs) != 2 || hbs[0].T != 1792000000 || hbs[1].T != 1792000060 || hbs[1].Up != 160 || len(alive) != 2 {
		t.Fatalf("unexpected: %+v %+v", hbs, alive)
	}
	if h, ok := st2.Latest(); !ok || h.T != 1792000060 {
		t.Fatalf("latest: %+v %v", h, ok)
	}
}

func TestParseLogIgnoresGarbage(t *testing.T) {
	data := []byte("garbage\nH,1792000000,5\nH,abc,5\nH,1,5\nT,1792000030\nH,1792000060\nH,1792000090,7,9\nT,1792000120")
	var hbs []hbEv
	var alive []int64
	parseLog(data, &hbs, &alive)
	if len(hbs) != 1 || len(alive) != 2 {
		t.Fatalf("hbs=%+v alive=%+v", hbs, alive)
	}
}

func TestStoreRefusesBadClock(t *testing.T) {
	st, _ := NewStore(t.TempDir(), time.UTC)
	if err := st.Append(time.Unix(0, 0), "T,0\n"); err == nil {
		t.Fatal("expected error for 1970 clock")
	}
	if !st.failing.Load() {
		t.Fatal("failing flag should be set")
	}
}

func newTestApp(t *testing.T) *App {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Makassar")
	if err != nil {
		t.Fatal(err)
	}
	st, err := NewStore(t.TempDir(), loc)
	if err != nil {
		t.Fatal(err)
	}
	return &App{store: st, loc: loc, token: "secret", downAfter: 180, started: time.Now().Unix() - 3600}
}

func do(app *App, method, url string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	app.routes().ServeHTTP(rec, httptest.NewRequest(method, url, nil))
	return rec
}

func TestHTTPFlow(t *testing.T) {
	app := newTestApp(t)

	if rec := do(app, "GET", "/hb"); rec.Code != http.StatusForbidden {
		t.Fatalf("missing key: %d", rec.Code)
	}
	if rec := do(app, "GET", "/hb?key=nope"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong key: %d", rec.Code)
	}
	if rec := do(app, "DELETE", "/hb?key=secret"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method: %d", rec.Code)
	}
	older := time.Now().Add(-50 * time.Second)
	if err := app.store.Append(older, fmt.Sprintf("H,%d,-1\n", older.Unix())); err != nil {
		t.Fatal(err)
	}
	if rec := do(app, "GET", "/hb?key=secret&up=1d02:03:04"); rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Fatalf("hb: %d %q", rec.Code, rec.Body.String())
	}
	if rec := do(app, "POST", "/hb?key=secret"); rec.Code != 200 { // no uptime is fine
		t.Fatalf("hb without uptime: %d", rec.Code)
	}

	rec := do(app, "GET", "/api/week")
	if rec.Code != 200 {
		t.Fatalf("week: %d %s", rec.Code, rec.Body.String())
	}
	var resp WeekResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status.State != "up" || len(resp.Days) != 7 || resp.Segments == nil || !resp.IsCurrent {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if resp.Stats.Availability == nil || *resp.Stats.Availability != 100 {
		t.Fatalf("availability: %+v", resp.Stats)
	}

	if rec := do(app, "GET", "/api/week?w=garbage"); rec.Code != 400 {
		t.Fatalf("bad week: %d", rec.Code)
	}
	if rec := do(app, "GET", "/api/week?w=2020-W10"); rec.Code != 200 {
		t.Fatalf("old week: %d", rec.Code)
	}
	if rec := do(app, "GET", "/api/status"); rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	if rec := do(app, "GET", "/healthz"); rec.Code != 200 {
		t.Fatalf("health: %d", rec.Code)
	}
	if rec := do(app, "GET", "/"); rec.Code != 200 || rec.Header().Get("Content-Type") != "text/html; charset=utf-8" || rec.Body.Len() < 1000 {
		t.Fatalf("index: %d", rec.Code)
	}
	if rec := do(app, "GET", "/nope"); rec.Code != 404 {
		t.Fatalf("404: %d", rec.Code)
	}
}

func TestEmptyServerIsHealthyAndNoData(t *testing.T) {
	app := newTestApp(t)
	rec := do(app, "GET", "/api/week")
	var resp WeekResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status.State != "nodata" || resp.Stats.Availability != nil || resp.DataFrom != 0 {
		t.Fatalf("unexpected: %+v", resp)
	}
}

func TestTokenOptional(t *testing.T) {
	app := newTestApp(t)
	app.token = ""
	if rec := do(app, "GET", "/hb"); rec.Code != 200 {
		t.Fatalf("no token configured should accept: %d", rec.Code)
	}
	if rec := do(app, "GET", "/boot"); rec.Code != 200 {
		t.Fatalf("no token configured should accept /boot: %d", rec.Code)
	}
}

func TestBootEndpoint(t *testing.T) {
	app := newTestApp(t)

	if rec := do(app, "GET", "/boot"); rec.Code != http.StatusForbidden {
		t.Fatalf("missing key on /boot: %d", rec.Code)
	}
	if rec := do(app, "GET", "/boot?key=wrong"); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong key on /boot: %d", rec.Code)
	}
	if rec := do(app, "DELETE", "/boot?key=secret"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("method on /boot: %d", rec.Code)
	}

	older := time.Now().Add(-50 * time.Second)
	if err := app.store.Append(older, fmt.Sprintf("H,%d,100\n", older.Unix())); err != nil {
		t.Fatal(err)
	}

	// Successful GET and POST boot calls
	if rec := do(app, "GET", "/boot?key=secret&up=15s"); rec.Code != 200 || rec.Body.String() != "ok\n" {
		t.Fatalf("boot GET: %d %q", rec.Code, rec.Body.String())
	}
	if app.getLastBoot() <= 0 {
		t.Fatalf("expected lastBoot to be recorded")
	}

	st := app.status(time.Now())
	if st.LastBoot <= 0 || st.LastBootStr == "" {
		t.Fatalf("status last_boot missing: %+v", st)
	}

	if rec := do(app, "POST", "/boot?key=secret&up=30s"); rec.Code != 200 {
		t.Fatalf("boot POST: %d", rec.Code)
	}

	// Verify week API shows the boot event
	rec := do(app, "GET", "/api/week")
	if rec.Code != 200 {
		t.Fatalf("week: %d", rec.Code)
	}
	var resp WeekResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Stats.Boots == 0 {
		t.Fatalf("expected boots > 0, got stats: %+v", resp.Stats)
	}
}

func TestAnalyzeBootEvent(t *testing.T) {
	hbs := beats(base, 5000, 3)
	last := hbs[len(hbs)-1]
	// Router boots 40 seconds later and sends explicit boot notification
	bootTime := last.T + 40
	hbs = append(hbs, hbEv{T: bootTime, Up: 10, Boot: true})
	segs := analyze(hbs, aliveOf(hbs), bootTime+10, 180)

	var bootFound bool
	for _, s := range segs {
		if s.Cause == CauseBoot {
			bootFound = true
			if s.E != bootTime {
				t.Fatalf("expected boot event at %d, got %+v", bootTime, s)
			}
		}
	}
	if !bootFound {
		t.Fatalf("expected CauseBoot segment, got %+v", segs)
	}
}

func TestAnalyzeBootAfterOutage(t *testing.T) {
	hbs := beats(base, 90000, 3)
	last := hbs[len(hbs)-1]
	// Router down for 10 minutes, boots up and sends boot notification
	bootTime := last.T + 600
	hbs = append(hbs, hbEv{T: bootTime, Up: 15, Boot: true})
	alive := aliveOf(hbs, ticks(last.T, bootTime)...)
	segs := analyze(hbs, alive, bootTime+10, 180)

	var powerFound, bootFound bool
	for _, s := range segs {
		if s.Cause == CausePower {
			powerFound = true
			if s.S != last.T || s.E != bootTime {
				t.Errorf("power segment wrong: %+v", s)
			}
		}
		if s.Cause == CauseBoot {
			bootFound = true
		}
	}
	if !powerFound || !bootFound {
		t.Fatalf("expected power outage and boot event, got: %+v", segs)
	}
}

func TestStoreLatestBoot(t *testing.T) {
	dir := t.TempDir()
	loc := time.UTC
	now := time.Date(2026, 10, 21, 5, 0, 0, 0, loc)

	st, err := NewStore(dir, loc)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(now, "H,1792000000,100\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(now, "B,1792000050,10\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.Append(now, "H,1792000100,60\n"); err != nil {
		t.Fatal(err)
	}

	boot, ok := st.LatestBoot()
	if !ok || boot != 1792000050 {
		t.Fatalf("expected LatestBoot 1792000050, got %d, %v", boot, ok)
	}
}
