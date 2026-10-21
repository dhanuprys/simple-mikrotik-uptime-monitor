package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // timezone database inside the binary (works in scratch images)
)

//go:embed index.html
var indexHTML []byte

type App struct {
	store     *Store
	loc       *time.Location
	token     string
	downAfter int64
	logHB     bool
	started   int64

	mu       sync.Mutex
	last     hbEv
	have     bool
	lastBoot int64
}

// ---- JSON shapes ------------------------------------------------------

type DayJSON struct {
	Label string `json:"label"`
	Start int64  `json:"start"`
	End   int64  `json:"end"`
}

type SegJSON struct {
	Segment
	From string `json:"from"`
	To   string `json:"to"`
	Dur  string `json:"dur"`
	DurS int64  `json:"dur_s"`
}

type Stats struct {
	Availability   *float64 `json:"availability"`
	Downtime       string   `json:"downtime"`
	Outages        int      `json:"outages"`
	Longest        string   `json:"longest"`
	Reboots        int      `json:"reboots"`
	Boots          int      `json:"boots"`
	MonitorOffline string   `json:"monitor_offline"`
}

type Status struct {
	State        string `json:"state"` // up | down | checking | nodata
	LastHB       int64  `json:"last_hb"`
	LastHBAgo    int64  `json:"last_hb_ago"`
	LastHBStr    string `json:"last_hb_str"`
	RouterUptime string `json:"router_uptime"`
	DownSince    string `json:"down_since"`
	DownFor      string `json:"down_for"`
	LastBoot     int64  `json:"last_boot,omitempty"`
	LastBootStr  string `json:"last_boot_str,omitempty"`
}

type WeekResp struct {
	Week      string    `json:"week"`
	Range     string    `json:"range"`
	TZ        string    `json:"tz"`
	Now       int64     `json:"now"`
	NowStr    string    `json:"now_str"`
	IsCurrent bool      `json:"is_current"`
	Prev      string    `json:"prev"`
	Next      string    `json:"next"`
	WeekStart int64     `json:"week_start"`
	WeekEnd   int64     `json:"week_end"`
	Days      []DayJSON `json:"days"`
	DataFrom  int64     `json:"data_from"`
	DataTo    int64     `json:"data_to"`
	DownAfter int64     `json:"down_after"`
	Segments  []SegJSON `json:"segments"`
	Stats     Stats     `json:"stats"`
	Status    Status    `json:"status"`
}

// ---- helpers ----------------------------------------------------------

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int64) int64 {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		log.Printf("invalid %s=%q, using %d", k, v, def)
		return def
	}
	return n
}

func (a *App) fmtTime(t int64) string {
	return time.Unix(t, 0).In(a.loc).Format("Mon 02 Jan 15:04")
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("json encode: %v", err)
	}
}

func (a *App) setLast(h hbEv) {
	a.mu.Lock()
	if !a.have || h.T >= a.last.T {
		a.last, a.have = h, true
	}
	a.mu.Unlock()
}

func (a *App) getLast() (hbEv, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last, a.have
}

func (a *App) setLastBoot(t int64) {
	a.mu.Lock()
	if t >= a.lastBoot {
		a.lastBoot = t
	}
	a.mu.Unlock()
}

func (a *App) getLastBoot() int64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastBoot
}

func (a *App) status(now time.Time) Status {
	lh, ok := a.getLast()
	if !ok {
		return Status{State: "nodata"}
	}
	ago := now.Unix() - lh.T
	if ago < 0 {
		ago = 0
	}
	st := Status{State: "up", LastHB: lh.T, LastHBAgo: ago, LastHBStr: a.fmtTime(lh.T)}
	if lh.Up >= 0 {
		st.RouterUptime = fmtDur(lh.Up)
	}
	if lb := a.getLastBoot(); lb > 0 {
		st.LastBoot = lb
		st.LastBootStr = a.fmtTime(lb)
	}
	if ago > a.downAfter {
		if now.Unix()-a.started < a.downAfter && lh.T < a.started {
			st.State = "checking" // monitor just started, give the router time to report
		} else {
			st.State = "down"
			st.DownSince = a.fmtTime(lh.T)
			st.DownFor = fmtDur(ago)
		}
	}
	return st
}

// ---- handlers ---------------------------------------------------------

func (a *App) handleHB(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if a.token != "" && subtle.ConstantTimeCompare([]byte(q.Get("key")), []byte(a.token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	up := parseUptime(q.Get("up"))
	now := time.Now()
	if err := a.store.Append(now, fmt.Sprintf("H,%d,%d\n", now.Unix(), up)); err != nil {
		log.Printf("heartbeat not stored: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.setLast(hbEv{T: now.Unix(), Up: up})
	if a.logHB {
		log.Printf("heartbeat from %s uptime=%ds", r.RemoteAddr, up)
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

func (a *App) handleBoot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	if a.token != "" && subtle.ConstantTimeCompare([]byte(q.Get("key")), []byte(a.token)) != 1 {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	up := parseUptime(q.Get("up"))
	now := time.Now()
	if err := a.store.Append(now, fmt.Sprintf("B,%d,%d\n", now.Unix(), up)); err != nil {
		log.Printf("boot event not stored: %v", err)
		http.Error(w, "storage error", http.StatusInternalServerError)
		return
	}
	a.setLast(hbEv{T: now.Unix(), Up: up, Boot: true})
	a.setLastBoot(now.Unix())
	if a.logHB {
		log.Printf("boot event from %s uptime=%ds", r.RemoteAddr, up)
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	writeJSON(w, map[string]any{"now": now.Unix(), "now_str": a.fmtTime(now.Unix()), "tz": a.loc.String(), "status": a.status(now)})
}

func (a *App) handleWeek(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	curKey := a.store.weekKey(now)
	key := r.URL.Query().Get("w")
	if key == "" {
		key = curKey
	}
	start, ok := a.store.weekStart(key)
	if !ok {
		http.Error(w, "bad week, expected e.g. 2026-W43", http.StatusBadRequest)
		return
	}
	end := start.AddDate(0, 0, 7)

	hbs, alive := a.store.Load(start)
	segs := analyze(hbs, alive, now.Unix(), a.downAfter)

	var dataFrom, dataTo int64
	if len(hbs) > 0 {
		dataFrom = max(start.Unix(), hbs[0].T)
		dataTo = min(end.Unix(), now.Unix())
		if dataTo <= dataFrom {
			dataFrom, dataTo = 0, 0
		}
	}

	days := make([]DayJSON, 7)
	for i := range days {
		ds := start.AddDate(0, 0, i)
		days[i] = DayJSON{Label: ds.Format("Mon 02 Jan"), Start: ds.Unix(), End: start.AddDate(0, 0, i+1).Unix()}
	}

	out := make([]SegJSON, 0, len(segs))
	var down, mon, longest int64
	var outages, reboots, boots int
	if dataFrom > 0 {
		for _, sg := range segs {
			if sg.Cause == CauseBoot || sg.Cause == CauseReboot {
				if sg.E < dataFrom || sg.E > dataTo {
					continue
				}
				if sg.Cause == CauseBoot {
					boots++
				} else {
					reboots++
				}
				js := SegJSON{
					Segment: Segment{S: sg.S, E: sg.E, Cause: sg.Cause},
					From:    a.fmtTime(sg.E),
				}
				out = append(out, js)
				continue
			}

			s, e := max(sg.S, dataFrom), min(sg.E, dataTo)
			if e <= s {
				continue
			}
			switch sg.Cause {
			case CauseMonitor:
				mon += e - s
			default:
				down += e - s
				outages++
				longest = max(longest, e-s)
			}
			js := SegJSON{
				Segment: Segment{S: s, E: e, Cause: sg.Cause, Ongoing: sg.Ongoing},
				From:    a.fmtTime(sg.S), To: a.fmtTime(sg.E), Dur: fmtDur(sg.E - sg.S), DurS: sg.E - sg.S,
			}
			if sg.Ongoing {
				js.To = "ongoing"
			}
			out = append(out, js)
		}
	}

	st := Stats{Outages: outages, Reboots: reboots, Boots: boots, Downtime: fmtDur(down), Longest: fmtDur(longest), MonitorOffline: fmtDur(mon)}
	if observed := dataTo - dataFrom - mon; dataFrom > 0 && observed > 0 {
		v := float64(observed-down) / float64(observed) * 100
		v = math.Floor(max(0, min(100, v))*100) / 100
		st.Availability = &v
	}

	resp := WeekResp{
		Week:      key,
		Range:     start.Format("02 Jan") + " – " + end.AddDate(0, 0, -1).Format("02 Jan 2006"),
		TZ:        a.loc.String(),
		Now:       now.Unix(),
		NowStr:    a.fmtTime(now.Unix()),
		IsCurrent: key == curKey,
		WeekStart: start.Unix(),
		WeekEnd:   end.Unix(),
		Days:      days,
		DataFrom:  dataFrom,
		DataTo:    dataTo,
		DownAfter: a.downAfter,
		Segments:  out,
		Stats:     st,
		Status:    a.status(now),
	}
	if keys := a.store.Keys(); len(keys) > 0 && keys[0] < key {
		resp.Prev = a.store.weekKey(start.AddDate(0, 0, -7))
	}
	if key < curKey {
		resp.Next = a.store.weekKey(end)
	}
	writeJSON(w, resp)
}

func (a *App) handleHealth(w http.ResponseWriter, r *http.Request) {
	if a.store.failing.Load() {
		http.Error(w, "storage failing", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(indexHTML)
}

func (a *App) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", handleIndex)
	mux.HandleFunc("/hb", a.handleHB)
	mux.HandleFunc("/boot", a.handleBoot)
	mux.HandleFunc("/api/week", a.handleWeek)
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/healthz", a.handleHealth)
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

// ---- liveness ticks ---------------------------------------------------

// The monitor writes a tick every 30s so that periods where the monitor
// itself was down are never mistaken for router outages.
func (a *App) writeTick() {
	now := time.Now()
	if err := a.store.Append(now, fmt.Sprintf("T,%d\n", now.Unix())); err != nil {
		log.Printf("tick not stored: %v", err)
	}
}

func (a *App) tickLoop(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.writeTick()
		}
	}
}

func healthcheck(port string) int {
	c := &http.Client{Timeout: 3 * time.Second}
	r, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	defer r.Body.Close()
	if r.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	port := env("PORT", "8080")
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck(port))
	}

	loc := time.UTC
	if tz := os.Getenv("TZ"); tz != "" {
		if l, err := time.LoadLocation(tz); err != nil {
			log.Printf("unknown TZ %q, using UTC: %v", tz, err)
		} else {
			loc = l
		}
	}

	downAfter := envInt("DOWN_AFTER", 180)
	if downAfter < 90 {
		log.Printf("DOWN_AFTER=%d is too small, using 90", downAfter)
		downAfter = 90
	}

	dir := env("DATA_DIR", "./data")
	store, err := NewStore(dir, loc)
	if err != nil {
		log.Fatalf("startup failed: %v", err)
	}

	app := &App{
		store:     store,
		loc:       loc,
		token:     os.Getenv("TOKEN"),
		downAfter: downAfter,
		logHB:     os.Getenv("LOG_HB") == "1",
		started:   time.Now().Unix(),
	}
	if h, ok := store.Latest(); ok {
		app.setLast(h)
	}
	if boot, ok := store.LatestBoot(); ok {
		app.setLastBoot(boot)
	}
	if app.token == "" {
		log.Printf("WARNING: TOKEN is empty, anyone who can reach /hb can record heartbeats")
	}
	app.writeTick()

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           app.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 14,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go app.tickLoop(ctx)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	log.Printf("listening on :%s, data=%s, tz=%s, down after %ds", port, dir, loc, downAfter)

	select {
	case err := <-errCh:
		log.Fatalf("server stopped: %v", err)
	case <-ctx.Done():
	}

	log.Printf("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
	app.writeTick()
	store.Close()
}
