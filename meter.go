package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"
)

const pairUserName = "local/p1-go-app"

// HTTPError is returned to the browser with a matching status code.
type HTTPError struct {
	Status int
	Msg    string
}

func (e *HTTPError) Error() string { return e.Msg }

// ---------- sample model ----------

type FieldKind int

const (
	Gauge   FieldKind = iota // instantaneous value, averaged when bucketed
	Counter                  // ever-increasing meter reading, differenced when bucketed
	State                    // last value wins
)

type Field struct {
	Name string
	Kind FieldKind
}

// Field indexes into Sample.V.
const (
	FPower = iota
	FPowerL1
	FPowerL2
	FPowerL3
	FVoltL1
	FVoltL2
	FVoltL3
	FAmpL1
	FAmpL2
	FAmpL3
	FFreq
	FImport
	FImportT1
	FImportT2
	FExport
	FExportT1
	FExportT2
	FGas
	FWater
	FTariff
	NF
)

var Fields = [NF]Field{
	{"power_w", Gauge}, {"power_l1_w", Gauge}, {"power_l2_w", Gauge}, {"power_l3_w", Gauge},
	{"voltage_l1_v", Gauge}, {"voltage_l2_v", Gauge}, {"voltage_l3_v", Gauge},
	{"current_l1_a", Gauge}, {"current_l2_a", Gauge}, {"current_l3_a", Gauge},
	{"frequency_hz", Gauge},
	{"energy_import_kwh", Counter}, {"energy_import_t1_kwh", Counter}, {"energy_import_t2_kwh", Counter},
	{"energy_export_kwh", Counter}, {"energy_export_t1_kwh", Counter}, {"energy_export_t2_kwh", Counter},
	{"gas_m3", Counter}, {"water_m3", Counter},
	{"tariff", State},
}

// Sample is one logged measurement (or, for history, one minute of them).
type Sample struct {
	T          int64   // unix seconds
	N          float64 // weight: number of raw samples represented
	V          [NF]float64
	PMin, PMax float64 // min/max of power_w inside the sample
}

func nan() float64 { return math.NaN() }

func newSample(t int64) Sample {
	s := Sample{T: t, N: 1, PMin: nan(), PMax: nan()}
	for i := range s.V {
		s.V[i] = nan()
	}
	return s
}

func numOf(m map[string]any, k string) float64 {
	if v, ok := m[k].(float64); ok && !math.IsInf(v, 0) && !math.IsNaN(v) {
		return v
	}
	return nan()
}

func firstOK(vs ...float64) float64 {
	for _, v := range vs {
		if !math.IsNaN(v) {
			return v
		}
	}
	return nan()
}

func sumOK(vs ...float64) float64 {
	sum, any := 0.0, false
	for _, v := range vs {
		if !math.IsNaN(v) {
			sum += v
			any = true
		}
	}
	if !any {
		return nan()
	}
	return sum
}

// SampleFromMeasurement maps the raw /api/measurement JSON to a Sample.
func SampleFromMeasurement(m map[string]any, t time.Time) Sample {
	s := newSample(t.Unix())
	n := func(k string) float64 { return numOf(m, k) }

	s.V[FPowerL1], s.V[FPowerL2], s.V[FPowerL3] = n("power_l1_w"), n("power_l2_w"), n("power_l3_w")
	s.V[FPower] = firstOK(n("power_w"), sumOK(s.V[FPowerL1], s.V[FPowerL2], s.V[FPowerL3]))
	s.V[FVoltL1] = firstOK(n("voltage_l1_v"), n("voltage_v"))
	s.V[FVoltL2], s.V[FVoltL3] = n("voltage_l2_v"), n("voltage_l3_v")
	s.V[FAmpL1] = firstOK(n("current_l1_a"), n("current_a"))
	s.V[FAmpL2], s.V[FAmpL3] = n("current_l2_a"), n("current_l3_a")
	s.V[FFreq] = n("frequency_hz")

	// Some meters only report per-tariff totals; fall back to their sum.
	s.V[FImportT1], s.V[FImportT2] = n("energy_import_t1_kwh"), n("energy_import_t2_kwh")
	s.V[FExportT1], s.V[FExportT2] = n("energy_export_t1_kwh"), n("energy_export_t2_kwh")
	s.V[FImport] = firstOK(n("energy_import_kwh"),
		sumOK(s.V[FImportT1], s.V[FImportT2], n("energy_import_t3_kwh"), n("energy_import_t4_kwh")))
	s.V[FExport] = firstOK(n("energy_export_kwh"),
		sumOK(s.V[FExportT1], s.V[FExportT2], n("energy_export_t3_kwh"), n("energy_export_t4_kwh")))
	s.V[FTariff] = n("tariff")

	if ext, ok := m["external"].([]any); ok {
		for _, e := range ext {
			em, _ := e.(map[string]any)
			switch em["type"] {
			case "gas_meter":
				if math.IsNaN(s.V[FGas]) {
					s.V[FGas] = numOf(em, "value")
				}
			case "water_meter":
				if math.IsNaN(s.V[FWater]) {
					s.V[FWater] = numOf(em, "value")
				}
			}
		}
	}
	s.PMin, s.PMax = s.V[FPower], s.V[FPower]
	return s
}

// ---------- meter client ----------

type Meter struct {
	cfg  *ConfigStore
	http *http.Client

	mu      sync.Mutex // the meter is a tiny ESP32: one request at a time
	last    map[string]any
	lastAt  time.Time
	lastErr string
	pairing bool
}

func NewMeter(cfg *ConfigStore) *Meter {
	tr := &http.Transport{
		// The meter's certificate is signed by HomeWizard's own CA and has no
		// real hostname, so (like `curl --insecure` / the Deno flag) it is not
		// verified. This only matters on a hostile LAN.
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: true},
		MaxIdleConns:          1,
		MaxIdleConnsPerHost:   1,
		IdleConnTimeout:       25 * time.Second,
		TLSHandshakeTimeout:   8 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
	}
	return &Meter{cfg: cfg, http: &http.Client{Transport: tr, Timeout: 10 * time.Second}}
}

type Reply struct {
	Status int
	Body   []byte
	Data   any // parsed JSON, or the body as string when not JSON
}

// doLocked performs one request. Caller must hold m.mu.
func (m *Meter) doLocked(ctx context.Context, method, path string, body any, auth bool, accept string) (*Reply, error) {
	c := m.cfg.Get()
	if c.Host == "" {
		return nil, &HTTPError{400, "No meter address saved yet."}
	}
	if auth && c.Token == "" {
		return nil, &HTTPError{401, "Not paired yet. Press “Pair” first."}
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "https://"+c.Host+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Version", "2")
	if auth {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	res, err := m.http.Do(req)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, &HTTPError{499, "Cancelled"}
		}
		return nil, &HTTPError{502, fmt.Sprintf("Could not reach %s: %v", c.Host, err)}
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	r := &Reply{Status: res.StatusCode, Body: b}
	if len(bytes.TrimSpace(b)) > 0 {
		var v any
		if json.Unmarshal(b, &v) == nil {
			r.Data = v
		} else {
			r.Data = string(b)
		}
	}
	return r, nil
}

// Do is a serialized request to the meter.
func (m *Meter) Do(ctx context.Context, method, path string, body any, auth bool, accept string) (*Reply, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doLocked(ctx, method, path, body, auth, accept)
}

// Measurement returns the latest measurement, re-using a result that is at
// most maxAge old so several loggers/viewers share one request.
func (m *Meter) Measurement(ctx context.Context, maxAge time.Duration) (map[string]any, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.last != nil && time.Since(m.lastAt) < maxAge {
		return m.last, m.lastAt, nil
	}
	r, err := m.doLocked(ctx, "GET", "/api/measurement", nil, true, "")
	if err != nil {
		m.lastErr = err.Error()
		return nil, time.Time{}, err
	}
	if r.Status == 401 {
		m.lastErr = "The meter rejected the token. Pair again."
		return nil, time.Time{}, &HTTPError{401, m.lastErr}
	}
	obj, ok := r.Data.(map[string]any)
	if r.Status != 200 || !ok {
		m.lastErr = fmt.Sprintf("Meter answered HTTP %d", r.Status)
		return nil, time.Time{}, &HTTPError{502, m.lastErr}
	}
	m.last, m.lastAt, m.lastErr = obj, time.Now(), ""
	return obj, m.lastAt, nil
}

// Last returns the most recent measurement without contacting the meter.
func (m *Meter) Last() (map[string]any, time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last, m.lastAt
}

func (m *Meter) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErr
}

// Pair keeps POSTing /api/user until the button on the meter is pressed.
func (m *Meter) Pair(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.pairing {
		m.mu.Unlock()
		return "", &HTTPError{409, "Pairing is already in progress."}
	}
	m.pairing = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.pairing = false; m.mu.Unlock() }()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", &HTTPError{499, "Cancelled"}
		}
		r, err := m.Do(ctx, "POST", "/api/user", map[string]string{"name": pairUserName}, false, "")
		if err != nil {
			return "", err
		}
		switch r.Status {
		case 200:
			d, _ := r.Data.(map[string]any)
			tok, _ := d["token"].(string)
			if tok == "" {
				return "", &HTTPError{502, "Meter returned 200 but no token."}
			}
			name, _ := d["name"].(string)
			if name == "" {
				name = pairUserName
			}
			err := m.cfg.Update(func(c *Config) {
				c.Token, c.UserName, c.PairedAt = tok, name, time.Now().Format(time.RFC3339)
			})
			return name, err
		case 403: // waiting for the button press
			select {
			case <-time.After(1500 * time.Millisecond):
			case <-ctx.Done():
			}
		default:
			return "", &HTTPError{502, fmt.Sprintf("Unexpected response %d: %s", r.Status, strings.TrimSpace(string(r.Body)))}
		}
	}
	return "", &HTTPError{408, "Timed out: the button on the meter was not pressed within 60 s."}
}
