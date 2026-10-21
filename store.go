package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var keyRe = regexp.MustCompile(`^(\d{4})-W(\d{2})$`)

// Store keeps one append-only text file per ISO week: <dir>/2026-W43.log
type Store struct {
	dir string
	loc *time.Location

	failing atomic.Bool // true while the last write failed

	mu   sync.Mutex
	f    *os.File
	fkey string
}

func NewStore(dir string, loc *time.Location) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	probe := filepath.Join(dir, ".write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return nil, fmt.Errorf("data directory %q is not writable (%v); fix the volume permissions or run the container with a matching --user", dir, err)
	}
	_ = os.Remove(probe)
	return &Store{dir: dir, loc: loc}, nil
}

func (s *Store) weekKey(t time.Time) string {
	y, w := t.In(s.loc).ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

// weekStart returns Monday 00:00 (in the configured zone) of an ISO week key.
func (s *Store) weekStart(key string) (time.Time, bool) {
	m := keyRe.FindStringSubmatch(key)
	if m == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1])
	w, _ := strconv.Atoi(m[2])
	if w < 1 || w > 53 || y < 2000 || y > 2100 {
		return time.Time{}, false
	}
	jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, s.loc) // always in ISO week 1
	off := (int(jan4.Weekday()) + 6) % 7          // days since Monday
	start := jan4.AddDate(0, 0, -off+(w-1)*7)
	if s.weekKey(start) != key { // e.g. week 53 in a 52-week year
		return time.Time{}, false
	}
	return start, true
}

// Append writes one line (must end with \n) to the file of the week of now.
func (s *Store) Append(now time.Time, line string) (err error) {
	defer func() { s.failing.Store(err != nil) }()
	if !plausible(now.Unix()) {
		return errors.New("system clock looks wrong, refusing to write")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	key := s.weekKey(now)
	if s.f == nil || s.fkey != key {
		if s.f != nil {
			_ = s.f.Close()
			s.f = nil
		}
		f, err := openLog(filepath.Join(s.dir, key+".log"))
		if err != nil {
			return err
		}
		s.f, s.fkey = f, key
	}
	if _, err := s.f.WriteString(line); err != nil {
		_ = s.f.Close()
		s.f = nil
		return err
	}
	if err := s.f.Sync(); err != nil {
		_ = s.f.Close()
		s.f = nil
		return err
	}
	return nil
}

func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f != nil {
		_ = s.f.Close()
		s.f = nil
	}
}

func openLog(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	if err := repairTail(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// repairTail removes a half-written last line (e.g. after power loss) so the
// next append starts on a clean line.
func repairTail(f *os.File) error {
	st, err := f.Stat()
	if err != nil {
		return err
	}
	size := st.Size()
	if size == 0 {
		return nil
	}
	n := int64(256)
	if size < n {
		n = size
	}
	buf := make([]byte, n)
	if got, err := f.ReadAt(buf, size-n); err != nil && !(errors.Is(err, io.EOF) && int64(got) == n) {
		return err
	}
	if buf[n-1] == '\n' {
		return nil
	}
	newSize := size - n
	if idx := bytes.LastIndexByte(buf, '\n'); idx >= 0 {
		newSize = size - n + int64(idx) + 1
	}
	return f.Truncate(newSize)
}

// Keys lists the week keys that have a data file, oldest first.
func (s *Store) Keys() []string {
	ents, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	var keys []string
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".log") {
			continue
		}
		if k := strings.TrimSuffix(n, ".log"); keyRe.MatchString(k) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Load returns the sorted events of the week before, the week itself and the
// week after, so outages crossing a week border are seen completely.
func (s *Store) Load(start time.Time) (hbs []hbEv, alive []int64) {
	for _, d := range []int{-7, 0, 7} {
		key := s.weekKey(start.AddDate(0, 0, d))
		data, err := os.ReadFile(filepath.Join(s.dir, key+".log"))
		if err != nil {
			continue // a missing week is normal
		}
		parseLog(data, &hbs, &alive)
	}
	sort.Slice(hbs, func(i, j int) bool { return hbs[i].T < hbs[j].T })
	sort.Slice(alive, func(i, j int) bool { return alive[i] < alive[j] })
	return hbs, alive
}

// Latest returns the newest heartbeat on disk.
func (s *Store) Latest() (hbEv, bool) {
	keys := s.Keys()
	for i := len(keys) - 1; i >= 0 && i >= len(keys)-4; i-- {
		data, err := os.ReadFile(filepath.Join(s.dir, keys[i]+".log"))
		if err != nil {
			continue
		}
		var hbs []hbEv
		var alive []int64
		parseLog(data, &hbs, &alive)
		if len(hbs) == 0 {
			continue
		}
		best := hbs[0]
		for _, h := range hbs {
			if h.T >= best.T {
				best = h
			}
		}
		return best, true
	}
	return hbEv{}, false
}

// LatestBoot returns the newest boot event timestamp on disk.
func (s *Store) LatestBoot() (int64, bool) {
	keys := s.Keys()
	for i := len(keys) - 1; i >= 0 && i >= len(keys)-4; i-- {
		data, err := os.ReadFile(filepath.Join(s.dir, keys[i]+".log"))
		if err != nil {
			continue
		}
		var hbs []hbEv
		var alive []int64
		parseLog(data, &hbs, &alive)
		var best int64
		var found bool
		for _, h := range hbs {
			if h.Boot && (!found || h.T >= best) {
				best, found = h.T, true
			}
		}
		if found {
			return best, true
		}
	}
	return 0, false
}
