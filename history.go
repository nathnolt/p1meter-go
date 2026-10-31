package main

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// F is a float that marshals NaN as null and rounds to 3 decimals.
type F float64

func (f F) MarshalJSON() ([]byte, error) {
	v := float64(f)
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return []byte("null"), nil
	}
	return strconv.AppendFloat(nil, math.Round(v*1000)/1000, 'f', -1, 64), nil
}

type Floats []F

func newFloats(n int) Floats {
	f := make(Floats, n)
	for i := range f {
		f[i] = F(math.NaN())
	}
	return f
}

type SeriesOut struct {
	T     []int64 `json:"t"`
	Power Floats  `json:"power"`
	PMin  Floats  `json:"pmin"`
	PMax  Floats  `json:"pmax"`
	L1    Floats  `json:"l1"`
	L2    Floats  `json:"l2"`
	L3    Floats  `json:"l3"`
	V1    Floats  `json:"v1"`
	V2    Floats  `json:"v2"`
	V3    Floats  `json:"v3"`
	A1    Floats  `json:"a1"`
	A2    Floats  `json:"a2"`
	A3    Floats  `json:"a3"`
	Hz    Floats  `json:"hz"`
	// Rd holds meter readings / state per point (only when requested): energy_*, gas_m3, water_m3, tariff
	Rd map[string]Floats `json:"rd,omitempty"`
	// Mn/Mx: lowest/highest value per bucket for the measurements asked for (variability band)
	Mn map[string]Floats `json:"mn,omitempty"`
	Mx map[string]Floats `json:"mx,omitempty"`
}

type UsageOut struct {
	Bucket   int64   `json:"bucket"`
	T        []int64 `json:"t"`
	Import   Floats  `json:"import"`
	Export   Floats  `json:"export"`
	ImportT1 Floats  `json:"import_t1"`
	ImportT2 Floats  `json:"import_t2"`
	Gas      Floats  `json:"gas"`
	Water    Floats  `json:"water"`
	Cover    Floats  `json:"cover"` // fraction of each bucket that has data
}

type Summary struct {
	Import   F     `json:"import_kwh"`
	Export   F     `json:"export_kwh"`
	ImportT1 F     `json:"import_t1_kwh"`
	ImportT2 F     `json:"import_t2_kwh"`
	Gas      F     `json:"gas_m3"`
	Water    F     `json:"water_m3"`
	AvgW     F     `json:"avg_w"`
	PeakW    F     `json:"peak_w"`
	PeakT    int64 `json:"peak_t"`
	MinW     F     `json:"min_w"`
	MinT     int64 `json:"min_t"`
	BaseW    F     `json:"base_w"`
	Samples  int   `json:"samples"`
	Coverage F     `json:"coverage"`
	FirstT   int64 `json:"first_t"`
	LastT    int64 `json:"last_t"`
}

// ---------- counters ----------

type counter struct {
	t []int64
	v []float64
}

func buildCounter(ss []Sample, f int) counter {
	var c counter
	for _, s := range ss {
		if !math.IsNaN(s.V[f]) {
			c.t = append(c.t, s.T)
			c.v = append(c.v, s.V[f])
		}
	}
	return c
}

// at returns the meter reading at time t, linearly interpolated between the
// surrounding samples and clamped to the first/last reading.
func (c counter) at(t int64) float64 {
	n := len(c.t)
	if n == 0 {
		return math.NaN()
	}
	if t <= c.t[0] {
		return c.v[0]
	}
	if t >= c.t[n-1] {
		return c.v[n-1]
	}
	i := sort.Search(n, func(i int) bool { return c.t[i] > t })
	t0, t1 := c.t[i-1], c.t[i]
	if t1 == t0 {
		return c.v[i]
	}
	return c.v[i-1] + (c.v[i]-c.v[i-1])*float64(t-t0)/float64(t1-t0)
}

// delta is the consumption in [a,b]; NaN when no data overlaps.
func (c counter) delta(a, b int64) float64 {
	n := len(c.t)
	if n == 0 || b <= c.t[0] || a >= c.t[n-1] {
		return math.NaN()
	}
	d := c.at(b) - c.at(a)
	if d < 0 { // meter replaced / reset
		d = 0
	}
	return d
}

// ---------- bucket helpers ----------

var niceBuckets = []int64{1, 2, 5, 10, 15, 30, 60, 120, 300, 600, 900, 1800, 3600, 7200, 14400, 21600, 43200, 86400}

func nextNice(b int64) int64 {
	for _, n := range niceBuckets {
		if n > b {
			return n
		}
	}
	return b * 2
}

func autoBucket(span, minBucket int64) int64 {
	want := span / 720
	for _, b := range niceBuckets {
		if b >= want && b >= minBucket {
			return b
		}
	}
	return niceBuckets[len(niceBuckets)-1]
}

// capBucket makes sure a range is not split into more than maxPts buckets.
func capBucket(b, span, maxPts int64) int64 {
	if b < 1 {
		b = 1
	}
	for span/b > maxPts && b < 86400 {
		b = nextNice(b)
	}
	return b
}

func usageAuto(span int64) int64 {
	switch {
	case span <= 15*60:
		return 60
	case span <= 90*60:
		return 300
	case span <= 8*3600:
		return 900
	case span <= 49*3600:
		return 3600
	}
	return 86400
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func inRange(ss []Sample, from, to int64) []Sample {
	lo := sort.Search(len(ss), func(i int) bool { return ss[i].T >= from })
	hi := sort.Search(len(ss), func(i int) bool { return ss[i].T >= to })
	if hi < lo {
		hi = lo
	}
	return ss[lo:hi]
}

// medianDt is the typical spacing between consecutive samples (0 if unknown).
func medianDt(ss []Sample) int64 {
	if len(ss) < 2 {
		return 0
	}
	step := 1 + len(ss)/2000
	var d []int64
	for i := step; i < len(ss); i += step {
		if x := ss[i].T - ss[i-step].T; x > 0 {
			d = append(d, x/int64(step))
		}
	}
	if len(d) == 0 {
		return 0
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	return d[len(d)/2]
}

// ---------- series & usage ----------

func newSeries(n int) SeriesOut {
	s := SeriesOut{T: make([]int64, n)}
	for _, p := range []*Floats{&s.Power, &s.PMin, &s.PMax, &s.L1, &s.L2, &s.L3, &s.V1, &s.V2, &s.V3, &s.A1, &s.A2, &s.A3, &s.Hz} {
		*p = newFloats(n)
	}
	return s
}

func seriesCols(s *SeriesOut) map[int]*Floats {
	return map[int]*Floats{
		FPower: &s.Power, FPowerL1: &s.L1, FPowerL2: &s.L2, FPowerL3: &s.L3,
		FVoltL1: &s.V1, FVoltL2: &s.V2, FVoltL3: &s.V3,
		FAmpL1: &s.A1, FAmpL2: &s.A2, FAmpL3: &s.A3, FFreq: &s.Hz,
	}
}

func newRd(n int) map[string]Floats {
	m := map[string]Floats{}
	for _, f := range Fields {
		if f.Kind != Gauge {
			m[f.Name] = newFloats(n)
		}
	}
	return m
}

func setRd(rd map[string]Floats, i int, s Sample) {
	for k, f := range Fields {
		if f.Kind != Gauge && !math.IsNaN(s.V[k]) {
			rd[f.Name][i] = F(s.V[k])
		}
	}
}

// SeriesFromSamples returns every sample as its own point (raw view).
func SeriesFromSamples(ss []Sample, readings bool) SeriesOut {
	out := newSeries(len(ss))
	cols := seriesCols(&out)
	if readings {
		out.Rd = newRd(len(ss))
	}
	for i, s := range ss {
		if readings {
			setRd(out.Rd, i, s)
		}
		out.T[i] = s.T
		for f, p := range cols {
			(*p)[i] = F(s.V[f])
		}
		out.PMin[i], out.PMax[i] = F(s.PMin), F(s.PMax)
	}
	return out
}

func alignDown(t, bucket int64, loc *time.Location) int64 {
	_, off := time.Unix(t, 0).In(loc).Zone()
	x := t + int64(off)
	x -= ((x % bucket) + bucket) % bucket
	return x - int64(off)
}

// BuildSeries averages gauges into fixed buckets between from and to.
func BuildSeries(ss []Sample, from, to, bucket int64, loc *time.Location, readings bool, ranges []int) SeriesOut {
	start := alignDown(from, bucket, loc)
	nb := int((to - start + bucket - 1) / bucket)
	if nb < 1 {
		nb = 1
	}
	out := newSeries(nb)
	if readings {
		out.Rd = newRd(nb)
	}
	if len(ranges) > 0 {
		out.Mn, out.Mx = map[string]Floats{}, map[string]Floats{}
		for _, f := range ranges {
			out.Mn[Fields[f].Name], out.Mx[Fields[f].Name] = newFloats(nb), newFloats(nb)
		}
	}
	for i := range out.T {
		out.T[i] = start + int64(i)*bucket
	}
	cols := seriesCols(&out)
	sum := map[int][]float64{}
	wgt := map[int][]float64{}
	for f := range cols {
		sum[f], wgt[f] = make([]float64, nb), make([]float64, nb)
	}
	for _, s := range ss {
		if s.T < from || s.T >= to {
			continue
		}
		i := int((s.T - start) / bucket)
		if readings {
			setRd(out.Rd, i, s)
		}
		for _, f := range ranges {
			lo, hi := s.V[f], s.V[f]
			if f == FPower { // power keeps the true min/max inside rolled-up minutes
				lo, hi = s.PMin, s.PMax
			}
			mn, mx := out.Mn[Fields[f].Name], out.Mx[Fields[f].Name]
			if !math.IsNaN(lo) && (math.IsNaN(float64(mn[i])) || lo < float64(mn[i])) {
				mn[i] = F(lo)
			}
			if !math.IsNaN(hi) && (math.IsNaN(float64(mx[i])) || hi > float64(mx[i])) {
				mx[i] = F(hi)
			}
		}
		for f := range cols {
			if v := s.V[f]; !math.IsNaN(v) {
				sum[f][i] += v * s.N
				wgt[f][i] += s.N
			}
		}
		if !math.IsNaN(s.PMin) && (math.IsNaN(float64(out.PMin[i])) || s.PMin < float64(out.PMin[i])) {
			out.PMin[i] = F(s.PMin)
		}
		if !math.IsNaN(s.PMax) && (math.IsNaN(float64(out.PMax[i])) || s.PMax > float64(out.PMax[i])) {
			out.PMax[i] = F(s.PMax)
		}
	}
	for f, p := range cols {
		for i := 0; i < nb; i++ {
			if wgt[f][i] > 0 {
				(*p)[i] = F(sum[f][i] / wgt[f][i])
			}
		}
	}
	return out
}

func floorDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func overlap(gaps [][2]int64, a, b int64) int64 {
	var sum int64
	for _, g := range gaps {
		lo, hi := max64(g[0], a), g[1]
		if hi > b {
			hi = b
		}
		if hi > lo {
			sum += hi - lo
		}
	}
	return sum
}

// BuildUsage computes consumption per bucket (any size; 86400 = local days).
func BuildUsage(ss []Sample, from, to time.Time, bucket int64, gaps [][2]int64) UsageOut {
	loc := from.Location()
	var starts, ends []int64
	if bucket >= 86400 {
		for t := floorDay(from); t.Before(to); t = t.AddDate(0, 0, 1) {
			starts = append(starts, t.Unix())
			ends = append(ends, t.AddDate(0, 0, 1).Unix())
		}
	} else {
		for t := alignDown(from.Unix(), bucket, loc); t < to.Unix(); t += bucket {
			starts = append(starts, t)
			ends = append(ends, t+bucket)
		}
	}
	n := len(starts)
	u := UsageOut{Bucket: bucket, T: starts,
		Import: newFloats(n), Export: newFloats(n), ImportT1: newFloats(n), ImportT2: newFloats(n),
		Gas: newFloats(n), Water: newFloats(n), Cover: newFloats(n)}
	type cf struct {
		f   int
		dst Floats
	}
	list := []cf{{FImport, u.Import}, {FExport, u.Export}, {FImportT1, u.ImportT1}, {FImportT2, u.ImportT2}, {FGas, u.Gas}, {FWater, u.Water}}
	counters := make([]counter, len(list))
	for i, c := range list {
		counters[i] = buildCounter(ss, c.f)
	}
	for i := range starts {
		a, b := max64(starts[i], from.Unix()), ends[i]
		if b > to.Unix() {
			b = to.Unix()
		}
		if b <= a {
			continue
		}
		for k, c := range list {
			c.dst[i] = F(counters[k].delta(a, b))
		}
		u.Cover[i] = F(1 - float64(overlap(gaps, a, b))/float64(b-a))
	}
	return u
}

// findGaps returns periods without samples that are longer than 4 sampling
// intervals (at least 3 minutes).
func findGaps(ss []Sample, from, to, dt int64) [][2]int64 {
	maxGap := max64(180, 4*dt)
	out := [][2]int64{}
	prev := from
	for _, s := range ss {
		if s.T < from || s.T >= to {
			continue
		}
		if s.T-prev > maxGap {
			out = append(out, [2]int64{prev, s.T})
		}
		prev = s.T
	}
	if to-prev > maxGap {
		out = append(out, [2]int64{prev, to})
	}
	return out
}

// Summarize computes totals over [from,to] from samples.
func Summarize(ss []Sample, from, to int64) Summary {
	nan := F(math.NaN())
	sm := Summary{Import: nan, Export: nan, ImportT1: nan, ImportT2: nan, Gas: nan, Water: nan,
		AvgW: nan, PeakW: nan, MinW: nan, BaseW: nan, Coverage: nan}
	in := inRange(ss, from, to)
	sm.Samples = len(in)
	if len(in) > 0 {
		sm.FirstT, sm.LastT = in[0].T, in[len(in)-1].T
	}
	set := func(dst *F, f int) { *dst = F(buildCounter(ss, f).delta(from, to)) }
	set(&sm.Import, FImport)
	set(&sm.Export, FExport)
	set(&sm.ImportT1, FImportT1)
	set(&sm.ImportT2, FImportT2)
	set(&sm.Gas, FGas)
	set(&sm.Water, FWater)

	var wsum, psum float64
	var powers []float64
	for _, s := range in {
		if p := s.V[FPower]; !math.IsNaN(p) {
			psum += p * s.N
			wsum += s.N
			if p >= 0 {
				powers = append(powers, p) // base load only looks at net-import samples
			}
		}
		if !math.IsNaN(s.PMax) && (math.IsNaN(float64(sm.PeakW)) || s.PMax > float64(sm.PeakW)) {
			sm.PeakW, sm.PeakT = F(s.PMax), s.T
		}
		if !math.IsNaN(s.PMin) && (math.IsNaN(float64(sm.MinW)) || s.PMin < float64(sm.MinW)) {
			sm.MinW, sm.MinT = F(s.PMin), s.T
		}
	}
	if wsum > 0 {
		sm.AvgW = F(psum / wsum)
	}
	if len(powers) > 0 {
		sort.Float64s(powers)
		sm.BaseW = F(powers[int(float64(len(powers)-1)*0.05)]) // 5th percentile ≈ standby load
	}
	return sm
}

// ---------- view ----------

type View struct {
	Bucket  int64      `json:"bucket"` // series bucket in seconds, 0 = one point per sample
	Dt      int64      `json:"dt"`     // typical spacing of the logged samples
	Series  *SeriesOut `json:"series,omitempty"`
	Usage   UsageOut   `json:"usage"`
	Summary Summary    `json:"summary"`
	Gaps    [][2]int64 `json:"gaps"` // periods with no logged data
}

// BuildView turns samples into everything the charts need. res: 0 = automatic,
// -1 = every sample, >0 = bucket size in seconds. minBucket is the smallest
// meaningful bucket (60 when the samples are minute rollups).
func BuildView(ss []Sample, from, to, dataTo time.Time, res, minBucket int64, lite, readings bool, ranges []int) View {
	f, t, dt := from.Unix(), to.Unix(), dataTo.Unix()
	in := inRange(ss, f, dt)
	d := medianDt(in)
	if d < minBucket {
		d = minBucket
	}
	if d < 1 {
		d = 1
	}
	v := View{Dt: d}
	v.Gaps = findGaps(in, f, dt, d)
	span := t - f
	if !lite {
		raw := minBucket <= 1 && (res == -1 || (res == 0 && len(in) <= 900))
		if raw && len(in) > 6000 {
			raw = false
		}
		if raw {
			s := SeriesFromSamples(in, readings)
			v.Series = &s
		} else {
			b := autoBucket(span, minBucket)
			switch {
			case res > 0:
				b = res
			case res == -1:
				b = d
			}
			b = capBucket(max64(max64(b, d), minBucket), span, 3000)
			s := BuildSeries(in, f, t, b, from.Location(), readings, ranges)
			v.Series, v.Bucket = &s, b
		}
	}
	ub := usageAuto(span)
	switch {
	case res > 0:
		ub = res
	case res == -1:
		ub = d
	}
	ub = capBucket(max64(max64(ub, d), minBucket), span, 3000)
	v.Usage = BuildUsage(ss, from, dataTo, ub, v.Gaps)
	v.Summary = Summarize(ss, f, dt)
	if dt > f {
		v.Summary.Coverage = F(1 - float64(overlap(v.Gaps, f, dt))/float64(dt-f))
	}
	return v
}

// ---------- range handling ----------

type RangeSpec struct {
	Key      string
	Label    string
	From, To time.Time // To = end of the axis (may be in the future)
	DataTo   time.Time // min(To, now)
}

func resolveRange(q url.Values, now time.Time) (RangeSpec, error) {
	loc := now.Location()
	today := floorDay(now)
	r := RangeSpec{Key: q.Get("range")}
	day := func(s string) (time.Time, error) { return time.ParseInLocation("2006-01-02", s, loc) }
	switch r.Key {
	case "", "today":
		r.Key, r.Label, r.From, r.To = "today", "Today", today, today.AddDate(0, 0, 1)
	case "yesterday":
		r.Label, r.From, r.To = "Yesterday", today.AddDate(0, 0, -1), today
	case "10m":
		r.Label, r.From, r.To = "Last 10 minutes", now.Add(-10*time.Minute), now
	case "1h":
		r.Label, r.From, r.To = "Last hour", now.Add(-time.Hour), now
	case "6h":
		r.Label, r.From, r.To = "Last 6 hours", now.Add(-6*time.Hour), now
	case "24h":
		r.Label, r.From, r.To = "Last 24 hours", now.Add(-24*time.Hour), now
	case "7d":
		r.Label, r.From, r.To = "Last 7 days", today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)
	case "30d":
		r.Label, r.From, r.To = "Last 30 days", today.AddDate(0, 0, -29), today.AddDate(0, 0, 1)
	case "date":
		d, err := day(q.Get("date"))
		if err != nil {
			return r, fmt.Errorf("date must be YYYY-MM-DD")
		}
		r.Label, r.From, r.To = d.Format("Mon 2 Jan 2006"), d, d.AddDate(0, 0, 1)
	case "custom":
		a, err1 := day(q.Get("from"))
		b, err2 := day(q.Get("to"))
		if err1 != nil || err2 != nil || b.Before(a) {
			return r, fmt.Errorf("from/to must be YYYY-MM-DD with from <= to")
		}
		r.Label, r.From, r.To = a.Format("2 Jan")+" – "+b.Format("2 Jan 2006"), a, b.AddDate(0, 0, 1)
	case "window": // a zoomed period, unix seconds
		t0, e0 := strconv.ParseInt(q.Get("t0"), 10, 64)
		t1, e1 := strconv.ParseInt(q.Get("t1"), 10, 64)
		if e0 != nil || e1 != nil || t1 <= t0 {
			return r, fmt.Errorf("t0/t1 must be unix seconds with t0 < t1")
		}
		r.Label, r.From, r.To = "Selected period", time.Unix(t0, 0).In(loc), time.Unix(t1, 0).In(loc)
	default:
		return r, fmt.Errorf("unknown range %q", r.Key)
	}
	if r.To.Sub(r.From) > 400*24*time.Hour {
		return r, fmt.Errorf("range too long (max 400 days)")
	}
	r.DataTo = r.To
	if now.Before(r.To) {
		r.DataTo = now
	}
	return r, nil
}

// parseRes: "" / "auto" = 0, "raw" = -1, otherwise seconds.
func parseRes(s string) (int64, error) {
	switch s {
	case "", "auto":
		return 0, nil
	case "raw":
		return -1, nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 || n > 86400 {
		return 0, fmt.Errorf("res must be auto, raw or a number of seconds")
	}
	return n, nil
}

type HistoryOut struct {
	Key   string `json:"range"`
	Label string `json:"label"`
	Date  string `json:"date,omitempty"` // set for single-day ranges
	From  int64  `json:"from"`
	To    int64  `json:"to"`
	Now   int64  `json:"now"`
	View
	Prev     *Summary `json:"prev,omitempty"`
	PrevFrom int64    `json:"prev_from,omitempty"`
	PrevTo   int64    `json:"prev_to,omitempty"`
}

func buildHistory(ms *MasterStore, r RangeSpec, now time.Time, res int64, lite, compare, readings bool, ranges []int) (*HistoryOut, error) {
	span := r.DataTo.Sub(r.From)
	if span < 0 {
		span = 0
	}
	useRaw := span <= 12*time.Hour || ((res == -1 || (res > 0 && res < 60)) && span <= 48*time.Hour)
	var ss []Sample
	var err error
	minB := int64(60)
	if useRaw {
		ss, err = ms.LoadRaw(r.From, r.DataTo)
		minB = 1
	} else {
		ss, err = ms.LoadRange(r.From, r.DataTo)
	}
	if err != nil {
		return nil, err
	}
	h := &HistoryOut{Key: r.Key, Label: r.Label, From: r.From.Unix(), To: r.To.Unix(), Now: now.Unix()}
	if r.Key == "today" || r.Key == "yesterday" || r.Key == "date" {
		h.Date = r.From.Format("2006-01-02")
	}
	h.View = BuildView(ss, r.From, r.To, r.DataTo, res, minB, lite, readings, ranges)
	if compare && r.To.Sub(r.From) >= 20*time.Hour {
		days := int(math.Round(r.To.Sub(r.From).Hours() / 24))
		if days < 1 {
			days = 1
		}
		pf, pt := r.From.AddDate(0, 0, -days), r.DataTo.AddDate(0, 0, -days)
		ps, err := ms.LoadRange(pf, pt)
		if err != nil {
			return nil, err
		}
		sm := Summarize(ps, pf.Unix(), pt.Unix())
		h.Prev, h.PrevFrom, h.PrevTo = &sm, pf.Unix(), pt.Unix()
	}
	return h, nil
}
