package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func sampleAt(t int64, power, imp, gas float64) Sample {
	s := newSample(t)
	s.V[FPower], s.V[FImport], s.V[FGas] = power, imp, gas
	s.PMin, s.PMax = power, power
	return s
}

func TestCounterDeltaInterpolatesAcrossGap(t *testing.T) {
	// 10 kWh at t=0, then the logger was down until t=3600 (13 kWh).
	ss := []Sample{sampleAt(0, 0, 10, math.NaN()), sampleAt(3600, 0, 13, math.NaN())}
	c := buildCounter(ss, FImport)
	if d := c.delta(0, 1800); math.Abs(d-1.5) > 1e-9 {
		t.Fatalf("half of the gap should hold half the usage, got %v", d)
	}
	if d := c.delta(4000, 5000); !math.IsNaN(d) {
		t.Fatalf("no data after the last sample must be NaN, got %v", d)
	}
}

func TestCounterResetClampsToZero(t *testing.T) {
	ss := []Sample{sampleAt(0, 0, 500, 1), sampleAt(60, 0, 3, 1)}
	if d := buildCounter(ss, FImport).delta(0, 60); d != 0 {
		t.Fatalf("meter replacement must not give negative usage, got %v", d)
	}
}

func TestCSVRoundTripAndRollup(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.csv")
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local).Unix()
	for i := 0; i < 24; i++ { // two minutes of 5 s samples
		s := sampleAt(base+int64(i*5), float64(100+i), 50+float64(i)/100, 7)
		cols := selectedCols(nil)
		if err := appendLine(p, cols.header(), cols.line(s)); err != nil {
			t.Fatal(err)
		}
	}
	// a half-written last line (power cut) must be ignored
	f, _ := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("2026-10-30T12:02:")
	f.Close()

	raw, err := ReadCSV(p)
	if err != nil || len(raw) != 24 {
		t.Fatalf("read: %v rows=%d", err, len(raw))
	}
	if raw[3].T != base+15 || raw[3].V[FPower] != 103 {
		t.Fatalf("bad row: %+v", raw[3])
	}
	mins := RollupMinutes(raw)
	if len(mins) != 2 {
		t.Fatalf("want 2 minute samples, got %d", len(mins))
	}
	if mins[0].N != 12 || mins[0].PMin != 100 || mins[0].PMax != 111 || math.Abs(mins[0].V[FPower]-105.5) > 1e-9 {
		t.Fatalf("bad rollup: %+v", mins[0])
	}
	if lt := lastRowTime(p); lt != base+23*5 {
		t.Fatalf("lastRowTime=%d want %d", lt, base+23*5)
	}
	if n := countDataLines(p); n != 24 {
		t.Fatalf("countDataLines=%d", n)
	}
}

func TestHourlyUsage(t *testing.T) {
	loc := time.Local
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, loc)
	var ss []Sample
	for m := 0; m <= 24*60; m++ { // 1 kWh per hour, gas 0.5 m3 per hour
		ts := day.Add(time.Duration(m) * time.Minute).Unix()
		ss = append(ss, sampleAt(ts, 1000, 100+float64(m)/60, 20+float64(m)/120))
	}
	u := BuildUsage(ss, day, day.AddDate(0, 0, 1), 3600, nil)
	if len(u.T) != 24 {
		t.Fatalf("want 24 hourly buckets, got %d", len(u.T))
	}
	for i := range u.T {
		if math.Abs(float64(u.Import[i])-1) > 1e-6 || math.Abs(float64(u.Gas[i])-0.5) > 1e-6 {
			t.Fatalf("hour %d: import=%v gas=%v", i, u.Import[i], u.Gas[i])
		}
	}
	sm := Summarize(ss, day.Unix(), day.AddDate(0, 0, 1).Unix())
	if math.Abs(float64(sm.Import)-24) > 1e-6 {
		t.Fatalf("summary: %+v", sm)
	}
}

func TestUsagePerFiveMinutes(t *testing.T) {
	day := time.Date(2026, 10, 30, 0, 0, 0, 0, time.Local)
	var ss []Sample
	for m := 0; m <= 60; m++ { // 1 kWh per hour
		ss = append(ss, sampleAt(day.Add(time.Duration(m)*time.Minute).Unix(), 1000, 100+float64(m)/60, math.NaN()))
	}
	u := BuildUsage(ss, day, day.Add(time.Hour), 300, nil)
	if len(u.T) != 12 {
		t.Fatalf("want 12 five-minute buckets, got %d", len(u.T))
	}
	for i := range u.T {
		if math.Abs(float64(u.Import[i])-1.0/12) > 1e-6 {
			t.Fatalf("bucket %d = %v", i, u.Import[i])
		}
	}
}

func TestGapsAndRawView(t *testing.T) {
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local).Unix()
	var ss []Sample
	for i := 0; i < 60; i++ { // 5 s samples for 5 min, then 20 min nothing, then 5 min more
		ss = append(ss, sampleAt(base+int64(i*5), 100, 10, math.NaN()))
	}
	for i := 0; i < 60; i++ {
		ss = append(ss, sampleAt(base+1500+int64(i*5), 100, 10.5, math.NaN()))
	}
	from, to := time.Unix(base, 0), time.Unix(base+1800, 0)
	v := BuildView(ss, from, to, to, -1, 1, false, false, nil)
	if v.Bucket != 0 || len(v.Series.T) != 120 {
		t.Fatalf("raw view should return every sample, bucket=%d n=%d", v.Bucket, len(v.Series.T))
	}
	if len(v.Gaps) != 1 || v.Gaps[0][1]-v.Gaps[0][0] < 1100 {
		t.Fatalf("gaps = %v", v.Gaps)
	}
	if c := float64(v.Summary.Coverage); c > 0.5 || c < 0.3 {
		t.Fatalf("coverage = %v", c)
	}
	// a slow logger (60 s interval) must not look like one big gap
	var slow []Sample
	for i := 0; i < 30; i++ {
		slow = append(slow, sampleAt(base+int64(i*60), 100, 10, math.NaN()))
	}
	if g := findGaps(slow, base, base+1800, 60); len(g) != 0 {
		t.Fatalf("unexpected gaps: %v", g)
	}
}

func TestColumnSelectionKeepsExistingHeader(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.csv")
	base := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local).Unix()
	few := selectedCols([]string{"power_w", "energy_import_kwh"})
	s := sampleAt(base, 100, 5, 7)
	appendLine(p, few.header(), few.line(s))
	// a later write with another selection must follow the file's own header
	hdr, ok := readHeaderCols(p)
	if !ok || len(hdr.names) != 2 {
		t.Fatalf("header: %v %v", hdr.names, ok)
	}
	s2 := sampleAt(base+5, 101, 5.1, 7)
	appendLine(p, hdr.header(), hdr.line(s2))
	// reshaping gives the file exactly the selected columns: import is dropped, gas is new
	if err := reshapeCSV(p, selectedCols([]string{"power_w", "gas_m3"})); err != nil {
		t.Fatal(err)
	}
	hdr2, _ := readHeaderCols(p)
	if len(hdr2.names) != 2 || hdr2.names[0] != "power_w" || hdr2.names[1] != "gas_m3" {
		t.Fatalf("columns after reshape: %v", hdr2.names)
	}
	rows, err := ReadCSV(p)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	if !math.IsNaN(rows[0].V[FGas]) || rows[1].V[FPower] != 101 || !math.IsNaN(rows[1].V[FImport]) {
		t.Fatalf("bad reshaped rows: %+v", rows)
	}
	// ... and new rows can now carry gas
	cols, _ := readHeaderCols(p)
	appendLine(p, cols.header(), cols.line(sampleAt(base+10, 102, 5.2, 8)))
	rows, _ = ReadCSV(p)
	if len(rows) != 3 || rows[2].V[FGas] != 8 {
		t.Fatalf("gas not recorded: %+v", rows[len(rows)-1])
	}
}

func TestPanelValidation(t *testing.T) {
	ps := NewPanelStore(filepath.Join(t.TempDir(), "panels.json"))
	if p, custom := ps.Get(); custom || len(p) != 4 {
		t.Fatalf("defaults expected, got %d custom=%v", len(p), custom)
	}
	bad := [][]string{{"nope"}, {"use:nothing"}, {"rd:power_w"}, {"power_w", "power_w"}, {}}
	for _, f := range bad {
		if _, err := ps.Set([]Panel{{Name: "x", Fields: f}}); err == nil {
			t.Fatalf("fields %v should be rejected", f)
		}
	}
	got, err := ps.Set([]Panel{{Name: " ", Type: "weird", Fields: []string{"power_w", "use:gas", "rd:gas_m3"}}})
	if err != nil || got[0].Name != "Panel" || got[0].Type != "line" || got[0].ID == "" {
		t.Fatalf("normalised panel: %+v err=%v", got, err)
	}
	if p, custom := ps.Get(); !custom || len(p) != 1 {
		t.Fatalf("saved panels not returned: %v", p)
	}
	if p, _ := ps.Reset(); len(p) != 4 {
		t.Fatal("reset should give defaults")
	}
}

func TestCalculatedFieldsAllowed(t *testing.T) {
	ok := []string{"calc:rate:gas_m3", "calc:delta:energy_import_kwh", "calc:diff:power_l1_w|power_l2_w", "calc:diff:use:import|use:export", "calc:diff:rd:gas_m3|rd:water_m3"}
	bad := []string{"calc:rate:power_w", "calc:rate:tariff", "calc:delta:nope", "calc:diff:power_w|power_w", "calc:diff:power_w|calc:rate:gas_m3", "calc:what:gas_m3", "calc:diff:power_w"}
	for _, id := range ok {
		if !allowedPanelField(id) {
			t.Errorf("%s should be allowed", id)
		}
	}
	for _, id := range bad {
		if allowedPanelField(id) {
			t.Errorf("%s should be rejected", id)
		}
	}
}

func TestCleanBaseline(t *testing.T) {
	b, err := cleanBaseline(Baseline{Enabled: true, Items: []BaselineItem{{ID: "x", Name: " Router ", Watts: 7.25, Enabled: true}, {ID: "x", Name: "Fridge", Watts: 40}}})
	if err != nil || b.Items[0].Name != "Router" || b.Items[0].ID == b.Items[1].ID {
		t.Fatalf("%+v %v", b, err)
	}
	if _, err := cleanBaseline(Baseline{Items: []BaselineItem{{Watts: -1}}}); err == nil {
		t.Fatal("negative watts accepted")
	}
}
