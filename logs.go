package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	ringSize     = 1440 // newest global-tracking samples kept in memory for the live chart
	liveRingSize = 7200
)

type LogMeta struct {
	ID           string     `json:"id"`
	Name         string     `json:"name"`
	Note         string     `json:"note,omitempty"`
	IntervalS    int        `json:"intervalS"`
	StopAfterMin int        `json:"stopAfterMin,omitempty"` // 0 = run until stopped
	Live         bool       `json:"live,omitempty"`         // created by the Live page
	File         string     `json:"file"`
	CreatedAt    time.Time  `json:"createdAt"`
	RunStartedAt time.Time  `json:"runStartedAt"`
	StoppedAt    *time.Time `json:"stoppedAt,omitempty"`
	Running      bool       `json:"running"`
}

type logRT struct {
	meta     LogMeta
	cancel   context.CancelFunc
	rows     int
	lastT    int64
	lastErr  string
	last     *Sample
	colsPath string // file whose header is in cols
	cols     Cols
	mask     []bool // columns of cols that are currently selected
}

// LogView is what the UI gets.
type LogView struct {
	LogMeta
	Master    bool     `json:"master"`
	Rows      int      `json:"rows"`
	Bytes     int64    `json:"bytes"`
	LastT     int64    `json:"lastT"`
	LastError string   `json:"lastError,omitempty"`
	Last      *LastVal `json:"last,omitempty"`
}

type LastVal struct {
	Power F `json:"power"`
	Imp   F `json:"import"`
	Exp   F `json:"export"`
	Gas   F `json:"gas"`
}

type LiveStatus struct {
	Active    bool   `json:"active"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	IntervalS int    `json:"intervalS,omitempty"`
	StartedAt int64  `json:"startedAt,omitempty"`
	Until     int64  `json:"until,omitempty"` // unix seconds, 0 = no limit
}

type Logs struct {
	meter    *Meter
	master   *MasterStore
	dir      string // <data>/logs
	mu       sync.Mutex
	m        *logRT // global tracking ("master")
	mcancel  context.CancelFunc
	user     map[string]*logRT
	ring     []Sample
	liveRing []Sample
	ctx      context.Context
	// OnLive is called for every poll of a live-session log (err != nil on failure).
	OnLive func(m map[string]any, at time.Time, err error)
}

func NewLogs(ctx context.Context, meter *Meter, logDir string) *Logs {
	return &Logs{ctx: ctx, meter: meter, dir: logDir, master: NewMasterStore(logDir), user: map[string]*logRT{}}
}

func (l *Logs) userDir() string    { return filepath.Join(l.dir, "user") }
func (l *Logs) indexPath() string  { return filepath.Join(l.userDir(), "index.json") }
func (l *Logs) tracking() Tracking { return l.meter.cfg.Get().Tracking }

// Start launches the global tracking and resumes user logs that were running
// when the app (or the Pi) went down.
func (l *Logs) Start() error {
	if err := os.MkdirAll(l.master.dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(l.userDir(), 0o755); err != nil {
		return err
	}
	l.loadRing()
	l.m = &logRT{meta: LogMeta{ID: "master", Name: "Global tracking", CreatedAt: time.Now()}}
	today := l.master.DayPath(time.Now())
	l.m.rows = countDataLines(today)
	l.m.lastT = lastRowTime(today)
	if l.m.lastT == 0 && len(l.ring) > 0 {
		l.m.lastT = l.ring[len(l.ring)-1].T
	}
	l.mu.Lock()
	l.applyMasterLocked()
	l.mu.Unlock()

	if b, err := os.ReadFile(l.indexPath()); err == nil {
		var metas []LogMeta
		if json.Unmarshal(b, &metas) == nil {
			for _, mt := range metas {
				rt := &logRT{meta: mt}
				p := filepath.Join(l.userDir(), mt.File)
				rt.rows = countDataLines(p)
				rt.lastT = lastRowTime(p)
				l.user[mt.ID] = rt
				if mt.Running {
					l.launch(rt)
				}
			}
		}
	}
	return nil
}

// applyMasterLocked (re)starts the global tracking with the configured interval.
func (l *Logs) applyMasterLocked() {
	if l.mcancel != nil {
		l.mcancel()
		l.mcancel = nil
	}
	tr := l.tracking()
	l.m.meta.IntervalS = tr.IntervalS
	l.m.meta.Running = tr.Enabled
	if tr.Enabled {
		ctx, cancel := context.WithCancel(l.ctx)
		l.mcancel = cancel
		go l.run(ctx, l.m, time.Duration(tr.IntervalS)*time.Second)
	}
}

// ApplyTracking is called after the tracking settings changed.
func (l *Logs) ApplyTracking() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	cols := selectedCols(l.tracking().Fields)
	// today's file gets exactly the selected columns (unneeded ones are dropped)
	var err error
	if l.tracking().Enabled {
		err = reshapeCSV(l.master.DayPath(time.Now()), cols)
	}
	l.m.colsPath = ""
	l.applyMasterLocked()
	return err
}

func (l *Logs) loadRing() {
	now := time.Now()
	var all []Sample
	for _, d := range []time.Time{now.AddDate(0, 0, -1), now} {
		if s, err := ReadCSV(l.master.DayPath(d)); err == nil {
			all = append(all, s...)
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].T < all[j].T })
	if len(all) > ringSize {
		all = all[len(all)-ringSize:]
	}
	l.ring = all
}

func (l *Logs) saveIndexLocked() {
	metas := make([]LogMeta, 0, len(l.user))
	for _, rt := range l.user {
		metas = append(metas, rt.meta)
	}
	sort.Slice(metas, func(i, j int) bool { return metas[i].CreatedAt.Before(metas[j].CreatedAt) })
	b, _ := json.MarshalIndent(metas, "", "  ")
	os.MkdirAll(l.userDir(), 0o755)
	tmp := l.indexPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		os.Rename(tmp, l.indexPath())
	}
}

// launch starts the sampling goroutine of a user log. Caller holds l.mu or is single-threaded.
func (l *Logs) launch(rt *logRT) {
	ctx, cancel := context.WithCancel(l.ctx)
	rt.cancel = cancel
	rt.meta.Running = true
	rt.meta.StoppedAt = nil
	if rt.meta.RunStartedAt.IsZero() {
		rt.meta.RunStartedAt = time.Now()
	}
	go l.run(ctx, rt, time.Duration(rt.meta.IntervalS)*time.Second)
}

var slugRe = regexp.MustCompile(`[^a-z0-9]+`)

func newID() string {
	b := make([]byte, 3)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (l *Logs) Create(name, note string, intervalS, stopAfterMin int, live bool) (*LogView, error) {
	if intervalS < 1 || intervalS > 3600 {
		return nil, &HTTPError{400, "Interval must be a whole number of seconds between 1 and 3600."}
	}
	if stopAfterMin < 0 || stopAfterMin > 60*24*30 {
		return nil, &HTTPError{400, "Auto-stop must be between 0 (never) and 43200 minutes."}
	}
	c := l.meter.cfg.Get()
	if c.Host == "" || c.Token == "" {
		return nil, &HTTPError{400, "Save the meter address and pair first."}
	}
	if err := os.MkdirAll(l.userDir(), 0o755); err != nil {
		return nil, err
	}
	l.mu.Lock()
	if live {
		for _, rt := range l.user {
			if rt.meta.Live && rt.meta.Running {
				l.mu.Unlock()
				return nil, &HTTPError{409, "Live tracking is already running."}
			}
		}
		l.liveRing = nil
	}
	now := time.Now()
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Log " + now.Format("2 Jan 15:04")
	}
	if len(name) > 80 {
		name = name[:80]
	}
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if live {
		slug = "live"
	}
	if slug == "" {
		slug = "log"
	}
	id := newID()
	// <givenname>-<timestamp>.csv, never overwriting an existing file
	base := fmt.Sprintf("%s-%s", slug, now.Format("20060102-150405"))
	file := base + ".csv"
	for i := 2; ; i++ {
		if _, err := os.Stat(filepath.Join(l.userDir(), file)); err != nil {
			break
		}
		file = fmt.Sprintf("%s-%d.csv", base, i)
	}
	rt := &logRT{meta: LogMeta{ID: id, Name: name, Note: strings.TrimSpace(note), IntervalS: intervalS,
		StopAfterMin: stopAfterMin, Live: live, File: file, CreatedAt: now, RunStartedAt: now}}
	l.user[id] = rt
	l.launch(rt)
	l.saveIndexLocked()
	v := l.view(rt)
	l.mu.Unlock()
	return &v, nil
}

func (l *Logs) Stop(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt := l.user[id]
	if rt == nil {
		return &HTTPError{404, "No such log."}
	}
	l.stopLocked(rt)
	l.saveIndexLocked()
	return nil
}

func (l *Logs) stopLocked(rt *logRT) {
	if rt.cancel != nil {
		rt.cancel()
		rt.cancel = nil
	}
	if rt.meta.Running {
		now := time.Now()
		rt.meta.Running, rt.meta.StoppedAt = false, &now
	}
}

func (l *Logs) Resume(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt := l.user[id]
	if rt == nil {
		return &HTTPError{404, "No such log."}
	}
	if rt.meta.Running {
		return &HTTPError{409, "Already running."}
	}
	rt.meta.RunStartedAt = time.Now()
	l.launch(rt)
	l.saveIndexLocked()
	return nil
}

func (l *Logs) Delete(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt := l.user[id]
	if rt == nil {
		return &HTTPError{404, "No such log."}
	}
	l.stopLocked(rt)
	os.Remove(filepath.Join(l.userDir(), rt.meta.File))
	delete(l.user, id)
	l.saveIndexLocked()
	return nil
}

func (l *Logs) Path(id string) (string, string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt := l.user[id]
	if rt == nil {
		return "", "", &HTTPError{404, "No such log."}
	}
	return filepath.Join(l.userDir(), rt.meta.File), rt.meta.Name, nil
}

func (l *Logs) Meta(id string) (LogMeta, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rt := l.user[id]
	if rt == nil {
		return LogMeta{}, false
	}
	return rt.meta, true
}

// LiveStatus reports the running live-tracking session, if any.
func (l *Logs) LiveStatus() LiveStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, rt := range l.user {
		if rt.meta.Live && rt.meta.Running {
			st := LiveStatus{Active: true, ID: rt.meta.ID, Name: rt.meta.Name, IntervalS: rt.meta.IntervalS, StartedAt: rt.meta.RunStartedAt.Unix()}
			if rt.meta.StopAfterMin > 0 {
				st.Until = rt.meta.RunStartedAt.Add(time.Duration(rt.meta.StopAfterMin) * time.Minute).Unix()
			}
			return st
		}
	}
	return LiveStatus{}
}

// ---------- sampling ----------

func (l *Logs) run(ctx context.Context, rt *logRT, iv time.Duration) {
	if rt.meta.ID != "master" {
		l.tick(rt) // first sample right away
	}
	for {
		// Wall-clock aligned ticks (…:00, :05, :10 for 5 s) so logs with the
		// same interval share one request to the meter.
		next := time.Now().Truncate(iv).Add(iv)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Until(next)):
		}
		if ctx.Err() != nil {
			return
		}
		l.tick(rt)
		if rt.meta.ID != "master" && rt.meta.StopAfterMin > 0 &&
			time.Since(rt.meta.RunStartedAt) >= time.Duration(rt.meta.StopAfterMin)*time.Minute {
			l.Stop(rt.meta.ID)
			return
		}
	}
}

func (l *Logs) tick(rt *logRT) {
	ctx, cancel := context.WithTimeout(l.ctx, 12*time.Second)
	defer cancel()
	m, at, err := l.meter.Measurement(ctx, 900*time.Millisecond)
	if rt.meta.Live && l.OnLive != nil {
		l.OnLive(m, at, err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err != nil {
		rt.lastErr = err.Error()
		return // a single failed poll is fine; try again next tick
	}
	s := SampleFromMeasurement(m, at)
	if math.IsNaN(s.V[FPower]) && math.IsNaN(s.V[FImport]) {
		rt.lastErr = "Measurement contained no power or energy values."
		return
	}
	if s.T < rt.lastT {
		// A Pi has no battery-backed clock: right after a power cut the time can
		// lag behind until NTP syncs. Never write rows out of order.
		rt.lastErr = "System clock is behind the last logged row (waiting for time sync?)"
		return
	}
	if s.T == rt.lastT {
		return
	}
	var path string
	if rt.meta.ID == "master" {
		path = l.master.DayPath(at)
		if path != rt.colsPath {
			rt.rows = countDataLines(path)
		}
	} else {
		path = filepath.Join(l.userDir(), rt.meta.File)
	}
	if path != rt.colsPath { // new file: keep the columns it already has
		if cols, ok := readHeaderCols(path); ok {
			rt.cols = cols
		} else {
			rt.cols = selectedCols(l.tracking().Fields)
		}
		rt.mask = maskFor(rt.cols, selectedCols(l.tracking().Fields))
		rt.colsPath = path
	}
	if err := appendLine(path, rt.cols.header(), rt.cols.lineMask(s, rt.mask)); err != nil {
		rt.lastErr = err.Error()
		return
	}
	rt.rows++
	rt.lastT = s.T
	rt.lastErr = ""
	cp := s
	rt.last = &cp
	if rt.meta.ID == "master" {
		l.ring = append(l.ring, s)
		if len(l.ring) > ringSize+60 {
			l.ring = append([]Sample(nil), l.ring[len(l.ring)-ringSize:]...)
		}
	}
	if rt.meta.Live {
		l.liveRing = append(l.liveRing, s)
		if len(l.liveRing) > liveRingSize+60 {
			l.liveRing = append([]Sample(nil), l.liveRing[len(l.liveRing)-liveRingSize:]...)
		}
	}
}

// ---------- views ----------

// view must be called with l.mu held.
func (l *Logs) view(rt *logRT) LogView {
	v := LogView{LogMeta: rt.meta, Master: rt.meta.ID == "master", Rows: rt.rows, LastT: rt.lastT, LastError: rt.lastErr}
	if rt.meta.ID == "master" {
		if st, err := os.Stat(l.master.DayPath(time.Now())); err == nil {
			v.Bytes = st.Size()
		}
	} else if st, err := os.Stat(filepath.Join(l.userDir(), rt.meta.File)); err == nil {
		v.Bytes = st.Size()
	}
	if rt.last != nil {
		v.Last = &LastVal{F(rt.last.V[FPower]), F(rt.last.V[FImport]), F(rt.last.V[FExport]), F(rt.last.V[FGas])}
	}
	return v
}

func (l *Logs) List() []LogView {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := []LogView{l.view(l.m)}
	us := make([]*logRT, 0, len(l.user))
	for _, rt := range l.user {
		us = append(us, rt)
	}
	sort.Slice(us, func(i, j int) bool { return us[i].meta.CreatedAt.After(us[j].meta.CreatedAt) })
	for _, rt := range us {
		out = append(out, l.view(rt))
	}
	return out
}

// Recent returns the newest raw samples (global tracking + live session)
// within the last n seconds, oldest first.
func (l *Logs) Recent(seconds int64) []Sample {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Unix() - seconds
	var all []Sample
	for _, src := range [][]Sample{l.ring, l.liveRing} {
		i := sort.Search(len(src), func(i int) bool { return src[i].T >= cut })
		all = append(all, src[i:]...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].T < all[j].T })
	out := all[:0]
	for _, s := range all {
		if len(out) > 0 && out[len(out)-1].T == s.T {
			continue
		}
		out = append(out, s)
	}
	return out
}
