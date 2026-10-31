package main

import (
	"bufio"
	"compress/gzip"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- CSV format ----------
//
//   timestamp,power_w,power_l1_w,...        (header, one column per Fields entry)
//   2026-10-31T14:32:05+01:00,412,...       (RFC 3339 local time with offset)
//
// Files are read by header name, so columns can be added in the future.

// Cols is an ordered set of CSV columns. Names that this version does not
// know are kept (idx -1) so appending to an old file never shifts columns.
type Cols struct {
	names []string
	idx   []int
}

var fieldIdx = func() map[string]int {
	m := map[string]int{}
	for i, f := range Fields {
		m[f.Name] = i
	}
	return m
}()

func newCols(names []string) Cols {
	c := Cols{names: names, idx: make([]int, len(names))}
	for i, n := range names {
		if j, ok := fieldIdx[n]; ok {
			c.idx[i] = j
		} else {
			c.idx[i] = -1
		}
	}
	return c
}

// selectedCols turns the configured field names into columns (empty = all).
func selectedCols(names []string) Cols {
	var keep []string
	for _, f := range Fields { // keep canonical order
		for _, n := range names {
			if n == f.Name {
				keep = append(keep, n)
			}
		}
	}
	if len(keep) == 0 {
		for _, f := range Fields {
			keep = append(keep, f.Name)
		}
	}
	return newCols(keep)
}

func (c Cols) header() string { return "timestamp," + strings.Join(c.names, ",") }

func (c Cols) line(s Sample) string { return c.lineMask(s, nil) }

// maskFor marks which of the file's columns are currently selected.
func maskFor(file, selected Cols) []bool {
	on := map[string]bool{}
	for _, n := range selected.names {
		on[n] = true
	}
	m := make([]bool, len(file.names))
	for i, n := range file.names {
		m[i] = on[n]
	}
	return m
}

// lineMask writes the row; columns outside the mask stay blank.
func (c Cols) lineMask(s Sample, mask []bool) string {
	var sb strings.Builder
	sb.WriteString(time.Unix(s.T, 0).Format(time.RFC3339))
	for i, j := range c.idx {
		sb.WriteByte(',')
		if j >= 0 && (mask == nil || mask[i]) {
			sb.WriteString(fmtNum(s.V[j]))
		}
	}
	sb.WriteByte('\n')
	return sb.String()
}

func fmtNum(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', -1, 64)
}

// readHeaderCols returns the columns of an existing, non-empty log file.
func readHeaderCols(path string) (Cols, bool) {
	f, err := os.Open(path)
	if err != nil {
		return Cols{}, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return Cols{}, false
	}
	parts := strings.Split(strings.TrimSpace(sc.Text()), ",")
	if len(parts) < 2 || parts[0] != "timestamp" {
		return Cols{}, false
	}
	return newCols(parts[1:]), true
}

// reshapeCSV rewrites a log file so that it has exactly the columns of want:
// columns that are no longer wanted are dropped, new ones start out blank.
func reshapeCSV(path string, want Cols) error {
	old, ok := readHeaderCols(path)
	if !ok {
		return nil
	}
	same := len(old.names) == len(want.names)
	for i := 0; same && i < len(old.names); i++ {
		same = old.names[i] == want.names[i]
	}
	if same {
		return nil
	}
	pos := map[string]int{}
	for i, n := range old.names {
		pos[n] = i
	}
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := path + ".tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	first := true
	for sc.Scan() {
		parts := strings.Split(sc.Text(), ",")
		if first {
			first = false
			w.WriteString(want.header() + "\n")
			continue
		}
		if len(parts) != len(old.names)+1 { // damaged line: drop
			continue
		}
		w.WriteString(parts[0])
		for _, n := range want.names {
			w.WriteByte(',')
			if i, ok := pos[n]; ok {
				w.WriteString(parts[i+1])
			}
		}
		w.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	w.Flush()
	out.Close()
	return os.Rename(tmp, path)
}

// ReadCSV parses a log file. Unparseable lines are skipped, so a half-written
// last line (power loss) never breaks reading.
func ReadCSV(path string) ([]Sample, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)

	var colIdx []int
	byName := map[string]int{}
	for i, fl := range Fields {
		byName[fl.Name] = i
	}
	var out []Sample
	var lastPrefix string
	var lastPrefixT int64
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if colIdx == nil {
			if parts[0] != "timestamp" {
				return nil, fmt.Errorf("%s: not a log file (missing header)", filepath.Base(path))
			}
			colIdx = make([]int, len(parts))
			for i, p := range parts {
				if j, ok := byName[strings.TrimSpace(p)]; ok {
					colIdx[i] = j
				} else {
					colIdx[i] = -1
				}
			}
			continue
		}
		if len(parts) != len(colIdx) {
			continue
		}
		// Timestamps within the same minute share a prefix; avoid re-parsing.
		var t int64
		if len(parts[0]) == 25 && parts[0][:16]+parts[0][19:] == lastPrefix {
			sec, err := strconv.Atoi(parts[0][17:19])
			if err != nil {
				continue
			}
			t = lastPrefixT + int64(sec)
		} else {
			tt, err := time.Parse(time.RFC3339, parts[0])
			if err != nil {
				continue
			}
			t = tt.Unix()
			if len(parts[0]) == 25 {
				lastPrefix, lastPrefixT = parts[0][:16]+parts[0][19:], t-int64(tt.Second())
			}
		}
		s := newSample(t)
		for i := 1; i < len(parts); i++ {
			j := colIdx[i]
			if j < 0 || parts[i] == "" {
				continue
			}
			if v, err := strconv.ParseFloat(parts[i], 64); err == nil {
				s.V[j] = v
			}
		}
		s.PMin, s.PMax = s.V[FPower], s.V[FPower]
		out = append(out, s)
	}
	return out, sc.Err()
}

func countDataLines(path string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	n := 0
	buf := make([]byte, 64*1024)
	for {
		c, err := f.Read(buf)
		for _, b := range buf[:c] {
			if b == '\n' {
				n++
			}
		}
		if err != nil {
			break
		}
	}
	if n > 0 {
		n-- // header
	}
	return n
}

// lastRowTime returns the timestamp of the last complete row (0 if none).
func lastRowTime(path string) int64 {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()
	st, _ := f.Stat()
	size := st.Size()
	n := int64(1024)
	if n > size {
		n = size
	}
	buf := make([]byte, n)
	if _, err := f.ReadAt(buf, size-n); err != nil && !errors.Is(err, io.EOF) {
		return 0
	}
	lines := strings.Split(strings.TrimRight(string(buf), "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		ts, _, _ := strings.Cut(lines[i], ",")
		if t, err := time.Parse(time.RFC3339, ts); err == nil {
			return t.Unix()
		}
	}
	return 0
}

func appendLine(path, header, line string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { // folder may be missing or deleted
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if st, err := f.Stat(); err == nil && st.Size() == 0 {
		if _, err := f.WriteString(header + "\n"); err != nil {
			return err
		}
	}
	_, err = f.WriteString(line)
	return err
}

// ---------- rollups ----------

// mergeSamples folds raw samples into one weighted sample: gauges are
// averaged, counters/state take the last value, power min/max are kept.
func mergeSamples(group []Sample) Sample {
	out := newSample(group[len(group)-1].T)
	out.N = 0
	var wsum [NF]float64
	var vsum [NF]float64
	for _, s := range group {
		out.N += s.N
		if !math.IsNaN(s.PMin) && (math.IsNaN(out.PMin) || s.PMin < out.PMin) {
			out.PMin = s.PMin
		}
		if !math.IsNaN(s.PMax) && (math.IsNaN(out.PMax) || s.PMax > out.PMax) {
			out.PMax = s.PMax
		}
		for i, v := range s.V {
			if math.IsNaN(v) {
				continue
			}
			if Fields[i].Kind == Gauge {
				vsum[i] += v * s.N
				wsum[i] += s.N
			} else {
				out.V[i] = v
			}
		}
	}
	for i := range out.V {
		if Fields[i].Kind == Gauge && wsum[i] > 0 {
			out.V[i] = vsum[i] / wsum[i]
		}
	}
	return out
}

// RollupMinutes groups time-sorted samples per minute.
func RollupMinutes(raw []Sample) []Sample {
	var out []Sample
	start := 0
	for i := 1; i <= len(raw); i++ {
		if i == len(raw) || raw[i].T/60 != raw[start].T/60 {
			out = append(out, mergeSamples(raw[start:i]))
			start = i
		}
	}
	return out
}

// ---------- master log store ----------

type dayCache struct {
	size    int64
	samples []Sample
	loaded  time.Time
}

type MasterStore struct {
	dir    string // daily csv files
	rollup string // gob.gz caches of finished days
	mu     sync.Mutex
	cache  map[string]*dayCache
	rmu    sync.Mutex
	raw    map[string]*dayCache
}

func NewMasterStore(logDir string) *MasterStore {
	return &MasterStore{
		dir:    filepath.Join(logDir, "master"),
		rollup: filepath.Join(logDir, "master", ".rollup"),
		cache:  map[string]*dayCache{},
		raw:    map[string]*dayCache{},
	}
}

func dayKey(t time.Time) string { return t.Format("2006-01-02") }

func (ms *MasterStore) DayPath(t time.Time) string {
	return filepath.Join(ms.dir, dayKey(t)+".csv")
}

type rollupFile struct {
	SrcSize int64
	Samples []Sample
}

func (ms *MasterStore) loadDay(day time.Time, now time.Time) ([]Sample, error) {
	key := dayKey(day)
	path := ms.DayPath(day)
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	isToday := key == dayKey(now)

	ms.mu.Lock()
	defer ms.mu.Unlock()
	if c := ms.cache[key]; c != nil && c.size == st.Size() && (!isToday || time.Since(c.loaded) < 15*time.Second) {
		return c.samples, nil
	}
	rpath := filepath.Join(ms.rollup, key+".gob.gz")
	if !isToday {
		if f, err := os.Open(rpath); err == nil {
			var rf rollupFile
			zr, zerr := gzip.NewReader(f)
			if zerr == nil {
				err = gob.NewDecoder(zr).Decode(&rf)
			} else {
				err = zerr
			}
			f.Close()
			if err == nil && rf.SrcSize == st.Size() {
				ms.cache[key] = &dayCache{size: st.Size(), samples: rf.Samples, loaded: time.Now()}
				return rf.Samples, nil
			}
		}
	}
	raw, err := ReadCSV(path)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(raw, func(i, j int) bool { return raw[i].T < raw[j].T })
	mins := RollupMinutes(raw)
	ms.cache[key] = &dayCache{size: st.Size(), samples: mins, loaded: time.Now()}
	if !isToday {
		if os.MkdirAll(ms.rollup, 0o755) == nil {
			tmp := rpath + ".tmp"
			if f, err := os.Create(tmp); err == nil {
				zw := gzip.NewWriter(f)
				werr := gob.NewEncoder(zw).Encode(rollupFile{SrcSize: st.Size(), Samples: mins})
				zw.Close()
				f.Close()
				if werr == nil {
					os.Rename(tmp, rpath)
				} else {
					os.Remove(tmp)
				}
			}
		}
	}
	// keep memory bounded (~60 days of minute samples)
	if len(ms.cache) > 60 {
		keys := make([]string, 0, len(ms.cache))
		for k := range ms.cache {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-60] {
			delete(ms.cache, k)
		}
	}
	return mins, nil
}

// LoadRange returns minute samples covering [from, to] plus the previous day
// (needed as baseline for the counters).
func (ms *MasterStore) LoadRange(from, to time.Time) ([]Sample, error) {
	now := time.Now()
	d := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location()).AddDate(0, 0, -1)
	var out []Sample
	for ; !d.After(to); d = d.AddDate(0, 0, 1) {
		s, err := ms.loadDay(d, now)
		if err != nil {
			return nil, err
		}
		out = append(out, s...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}

type DayFile struct {
	Date string `json:"date"`
	Size int64  `json:"size"`
}

func (ms *MasterStore) Files() []DayFile {
	ents, _ := os.ReadDir(ms.dir)
	var out []DayFile
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".csv") {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, DayFile{Date: strings.TrimSuffix(n, ".csv"), Size: info.Size()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Date > out[j].Date })
	return out
}

func (ms *MasterStore) loadDayRaw(day, now time.Time) ([]Sample, error) {
	key := dayKey(day)
	path := ms.DayPath(day)
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	ms.rmu.Lock()
	defer ms.rmu.Unlock()
	isToday := key == dayKey(now)
	if c := ms.raw[key]; c != nil && c.size == st.Size() && (!isToday || time.Since(c.loaded) < 3*time.Second) {
		return c.samples, nil
	}
	s, err := ReadCSV(path)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].T < s[j].T })
	ms.raw[key] = &dayCache{size: st.Size(), samples: s, loaded: time.Now()}
	if len(ms.raw) > 4 { // raw days are big: keep only the newest few
		keys := make([]string, 0, len(ms.raw))
		for k := range ms.raw {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys[:len(keys)-4] {
			delete(ms.raw, k)
		}
	}
	return s, nil
}

// LoadRaw returns the logged samples (no rollup) in [from-2h, to].
func (ms *MasterStore) LoadRaw(from, to time.Time) ([]Sample, error) {
	now := time.Now()
	lo, hi := from.Add(-2*time.Hour).Unix(), to.Unix()
	d := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, from.Location()).AddDate(0, 0, -1)
	var out []Sample
	for ; !d.After(to); d = d.AddDate(0, 0, 1) {
		s, err := ms.loadDayRaw(d, now)
		if err != nil {
			return nil, err
		}
		i := sort.Search(len(s), func(i int) bool { return s[i].T >= lo })
		for ; i < len(s) && s[i].T <= hi; i++ {
			out = append(out, s[i])
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].T < out[j].T })
	return out, nil
}
