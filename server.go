package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

type Server struct {
	cfg    *ConfigStore
	meter  *Meter
	logs   *Logs
	hub    *Hub
	panels *PanelStore
	base   *BaselineStore
	auth   string // "user:pass" or ""
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(b)
}

func writeErr(w http.ResponseWriter, err error) {
	var he *HTTPError
	if errors.As(err, &he) {
		writeJSON(w, he.Status, map[string]any{"ok": false, "error": he.Msg})
		return
	}
	writeJSON(w, 500, map[string]any{"ok": false, "error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	r.Body = http.MaxBytesReader(nil, r.Body, 1<<16)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return &HTTPError{400, "Invalid JSON body."}
	}
	return nil
}

// handle wraps a handler returning an error.
func handle(f func(w http.ResponseWriter, r *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := f(w, r); err != nil {
			writeErr(w, err)
		}
	}
}

// ---------- middleware ----------

func (s *Server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.auth != "" {
			u, p, ok := r.BasicAuth()
			want := strings.SplitN(s.auth, ":", 2)
			if !ok || len(want) != 2 ||
				subtle.ConstantTimeCompare([]byte(u), []byte(want[0])) != 1 ||
				subtle.ConstantTimeCompare([]byte(p), []byte(want[1])) != 1 {
				w.Header().Set("WWW-Authenticate", `Basic realm="P1 meter"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		// CSRF guard: a state-changing request must come from this very site.
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- live hub (Server-Sent Events) ----------

type Hub struct {
	meter *Meter
	mu    sync.Mutex
	subs  map[chan []byte]struct{}
}

func NewHub(m *Meter) *Hub { return &Hub{meter: m, subs: map[chan []byte]struct{}{}} }

func (h *Hub) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

func (h *Hub) broadcast(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- b:
		default: // slow client: drop
		}
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	ch := make(chan []byte, 4)
	s.hub.mu.Lock()
	s.hub.subs[ch] = struct{}{}
	s.hub.mu.Unlock()
	defer func() {
		s.hub.mu.Lock()
		delete(s.hub.subs, ch)
		s.hub.mu.Unlock()
	}()
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ka := time.NewTicker(15 * time.Second)
	defer ka.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ka.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		}
	}
}

// ---------- device proxy ----------

func (s *Server) deviceCall(ctx context.Context, method, path string, body any, accept string) (map[string]any, error) {
	r, err := s.meter.Do(ctx, method, path, body, true, accept)
	if err != nil {
		return nil, err
	}
	res := map[string]any{"ok": r.Status >= 200 && r.Status < 300, "status": r.Status, "data": r.Data}
	if r.Status == 401 {
		res["error"] = "The meter rejected the token (user:unauthorized). Pair again."
	} else if !res["ok"].(bool) {
		if d, ok := r.Data.(map[string]any); ok {
			if e, ok := d["error"].(string); ok {
				res["error"] = e
			}
		}
		if _, ok := res["error"]; !ok {
			res["error"] = fmt.Sprintf("Meter answered HTTP %d", r.Status)
		}
	}
	return res, nil
}

func (s *Server) proxy(method, path, accept string) http.HandlerFunc {
	return handle(func(w http.ResponseWriter, r *http.Request) error {
		res, err := s.deviceCall(r.Context(), method, path, nil, accept)
		if err != nil {
			return err
		}
		writeJSON(w, 200, res)
		return nil
	})
}

var userNameRe = regexp.MustCompile(`^(local|cloud)/[a-zA-Z0-9\-_/\\# ]{1,40}$`)

func (s *Server) putSystem(w http.ResponseWriter, r *http.Request) error {
	var in map[string]any
	if err := readJSON(r, &in); err != nil {
		return err
	}
	out := map[string]any{}
	for k, v := range in {
		switch k {
		case "cloud_enabled", "api_v1_enabled":
			b, ok := v.(bool)
			if !ok {
				return &HTTPError{400, k + " must be true or false."}
			}
			out[k] = b
		case "status_led_brightness_pct":
			n, ok := v.(float64)
			if !ok || n < 0 || n > 100 || n != math.Trunc(n) {
				return &HTTPError{400, "LED brightness must be a whole number from 0 to 100."}
			}
			out[k] = n
		default:
			return &HTTPError{400, "Setting not supported: " + k}
		}
	}
	if len(out) == 0 {
		return &HTTPError{400, "Nothing to change."}
	}
	res, err := s.deviceCall(r.Context(), "PUT", "/api/system", out, "")
	if err != nil {
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) putBatteries(w http.ResponseWriter, r *http.Request) error {
	var in map[string]any
	if err := readJSON(r, &in); err != nil {
		return err
	}
	out := map[string]any{}
	for k, v := range in {
		switch k {
		case "mode":
			m, _ := v.(string)
			if m != "zero" && m != "standby" && m != "predictive" && m != "to_full" {
				return &HTTPError{400, "mode must be zero, standby, predictive or to_full."}
			}
			out[k] = m
		case "charge_to_full":
			b, ok := v.(bool)
			if !ok {
				return &HTTPError{400, "charge_to_full must be true or false."}
			}
			out[k] = b
		case "permissions":
			arr, ok := v.([]any)
			if !ok {
				return &HTTPError{400, "permissions must be a list."}
			}
			perms := []string{}
			for _, p := range arr {
				ps, _ := p.(string)
				if ps != "charge_allowed" && ps != "discharge_allowed" {
					return &HTTPError{400, "Unknown permission: " + fmt.Sprint(p)}
				}
				perms = append(perms, ps)
			}
			out[k] = perms
		default:
			return &HTTPError{400, "Setting not supported: " + k}
		}
	}
	if len(out) == 0 {
		return &HTTPError{400, "Nothing to change."}
	}
	res, err := s.deviceCall(r.Context(), "PUT", "/api/batteries", out, "")
	if err != nil {
		return err
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name  string `json:"name"`
		Force bool   `json:"force"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if !userNameRe.MatchString(in.Name) {
		return &HTTPError{400, "Invalid user name."}
	}
	list, err := s.deviceCall(r.Context(), "GET", "/api/user", nil, "")
	if err != nil {
		return err
	}
	isCurrent := false
	if arr, ok := list["data"].([]any); ok {
		for _, u := range arr {
			if um, ok := u.(map[string]any); ok && um["name"] == in.Name && um["current"] == true {
				isCurrent = true
			}
		}
	}
	if isCurrent && !in.Force {
		return &HTTPError{409, "That is the user this app logs in with. Deleting it cuts off this app (you would have to pair again)."}
	}
	res, err := s.deviceCall(r.Context(), "DELETE", "/api/user", map[string]string{"name": in.Name}, "")
	if err != nil {
		return err
	}
	if res["ok"] == true && isCurrent {
		s.cfg.Update(func(c *Config) { c.Token, c.UserName, c.PairedAt = "", "", "" })
	}
	writeJSON(w, 200, res)
	return nil
}

func (s *Server) all(w http.ResponseWriter, r *http.Request) error {
	endpoints := []struct{ name, path, accept string }{
		{"device", "/api", ""}, {"measurement", "/api/measurement", ""}, {"system", "/api/system", ""},
		{"telegram", "/api/telegram", "text/plain"}, {"batteries", "/api/batteries", ""}, {"users", "/api/user", ""},
	}
	results := map[string]any{}
	for _, e := range endpoints { // sequential: the meter handles one request at a time
		res, err := s.deviceCall(r.Context(), "GET", e.path, nil, e.accept)
		if err != nil {
			res = map[string]any{"ok": false, "status": 0, "error": err.Error()}
		}
		res["path"] = e.path
		results[e.name] = res
	}
	writeJSON(w, 200, map[string]any{"ok": true, "results": results})
	return nil
}

// ---------- config / pairing ----------

var hostRe = regexp.MustCompile(`^[a-zA-Z0-9.\-]{1,253}(:[0-9]{1,5})?$`)

func (s *Server) info(w http.ResponseWriter, r *http.Request) error {
	c := s.cfg.Get()
	now := time.Now()
	zone, off := now.Zone()
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": version, "now": now.Unix(), "tz": zone, "tzOffset": off,
		"host": c.Host, "paired": c.Token != "", "userName": c.UserName, "pairedAt": c.PairedAt,
		"prices": c.Prices, "labels": c.Labels, "meterError": s.meter.LastError(),
	})
	return nil
}

func (s *Server) saveHost(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Host string `json:"host"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	h := strings.TrimSpace(in.Host)
	if !hostRe.MatchString(h) {
		return &HTTPError{400, "Enter an IP address or hostname (no path)."}
	}
	return s.setHost(w, h)
}

func (s *Server) setHost(w http.ResponseWriter, h string) error {
	err := s.cfg.Update(func(c *Config) {
		if c.Host != "" && c.Host != h { // a token belongs to one meter
			c.Token, c.UserName, c.PairedAt = "", "", ""
		}
		c.Host = h
	})
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true})
	return nil
}

func (s *Server) savePrices(w http.ResponseWriter, r *http.Request) error {
	var p Prices
	if err := readJSON(r, &p); err != nil {
		return err
	}
	for _, v := range []float64{p.Import, p.Export, p.Gas} {
		if math.IsNaN(v) || v < -100 || v > 1000 {
			return &HTTPError{400, "Prices must be sensible numbers."}
		}
	}
	p.Currency = strings.TrimSpace(p.Currency)
	if p.Currency == "" || len(p.Currency) > 4 {
		p.Currency = "€"
	}
	if err := s.cfg.Update(func(c *Config) { c.Prices = p }); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "prices": p})
	return nil
}

var labelKeyRe = regexp.MustCompile(`^[a-z0-9_:.]{1,48}$`)

func (s *Server) saveLabels(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Labels map[string]string `json:"labels"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if len(in.Labels) > 120 {
		return &HTTPError{400, "Too many labels."}
	}
	clean := map[string]string{}
	for k, v := range in.Labels {
		v = strings.TrimSpace(v)
		if !labelKeyRe.MatchString(k) || len(v) > 40 {
			return &HTTPError{400, "Labels must be at most 40 characters."}
		}
		if v != "" {
			clean[k] = v
		}
	}
	if err := s.cfg.Update(func(c *Config) { c.Labels = clean }); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "labels": clean})
	return nil
}

// parseRanges turns "power_w,voltage_l1_v" into field indexes (measurements only).
func parseRanges(q string) []int {
	var out []int
	for _, n := range strings.Split(q, ",") {
		if i, ok := fieldIdx[strings.TrimSpace(n)]; ok && Fields[i].Kind == Gauge {
			out = append(out, i)
		}
	}
	return out
}

// ---------- history / logs ----------

type LiveOut struct {
	T     []int64 `json:"t"`
	Power Floats  `json:"power"`
	L1    Floats  `json:"l1"`
	L2    Floats  `json:"l2"`
	L3    Floats  `json:"l3"`
}

func (s *Server) live(w http.ResponseWriter, r *http.Request) error {
	secs, _ := strconv.ParseInt(r.URL.Query().Get("seconds"), 10, 64)
	if secs <= 0 || secs > 7200 {
		secs = 900
	}
	ss := s.logs.Recent(secs)
	o := LiveOut{T: make([]int64, len(ss)), Power: newFloats(len(ss)), L1: newFloats(len(ss)), L2: newFloats(len(ss)), L3: newFloats(len(ss))}
	for i, x := range ss {
		o.T[i] = x.T
		o.Power[i], o.L1[i], o.L2[i], o.L3[i] = F(x.V[FPower]), F(x.V[FPowerL1]), F(x.V[FPowerL2]), F(x.V[FPowerL3])
	}
	writeJSON(w, 200, o)
	return nil
}

func (s *Server) history(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	now := time.Now()
	rs, err := resolveRange(q, now)
	if err != nil {
		return &HTTPError{400, err.Error()}
	}
	res, err := parseRes(q.Get("res"))
	if err != nil {
		return &HTTPError{400, err.Error()}
	}
	h, err := buildHistory(s.logs.master, rs, now, res, q.Get("lite") == "1", q.Get("compare") == "1", q.Get("readings") == "1", parseRanges(q.Get("ranges")))
	if err != nil {
		return err
	}
	writeJSON(w, 200, h)
	return nil
}

func (s *Server) listLogs(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, 200, map[string]any{"ok": true, "logs": s.logs.List(), "masterFiles": s.logs.master.Files()})
	return nil
}

func (s *Server) createLog(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name         string `json:"name"`
		Note         string `json:"note"`
		IntervalS    int    `json:"intervalS"`
		StopAfterMin int    `json:"stopAfterMin"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	v, err := s.logs.Create(in.Name, in.Note, in.IntervalS, in.StopAfterMin, false)
	if err != nil {
		return err
	}
	writeJSON(w, 200, map[string]any{"ok": true, "log": v})
	return nil
}

func (s *Server) logData(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("id")
	path, _, err := s.logs.Path(id)
	if err != nil {
		return err
	}
	meta, _ := s.logs.Meta(id)
	q := r.URL.Query()
	res, err := parseRes(q.Get("res"))
	if err != nil {
		return &HTTPError{400, err.Error()}
	}
	ss, err := ReadCSV(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	sort.SliceStable(ss, func(i, j int) bool { return ss[i].T < ss[j].T })
	out := map[string]any{"ok": true, "meta": meta, "rows": len(ss)}
	if len(ss) > 0 {
		first, last := ss[0].T, ss[len(ss)-1].T
		from, to := first, last+1
		if v, e := strconv.ParseInt(q.Get("t0"), 10, 64); e == nil && v > 0 {
			from = v
		}
		if v, e := strconv.ParseInt(q.Get("t1"), 10, 64); e == nil && v > from {
			to = v
		}
		v := BuildView(ss, time.Unix(from, 0), time.Unix(to, 0), time.Unix(to, 0), res, 1, false, q.Get("readings") == "1", parseRanges(q.Get("ranges")))
		out["view"] = v
		out["from"], out["to"], out["first"], out["last"] = from, to, first, last
	}
	writeJSON(w, 200, out)
	return nil
}

// ---------- tracking & live ----------

func (s *Server) trackingGet(w http.ResponseWriter, r *http.Request) error {
	t := s.cfg.Get().Tracking
	names := make([]string, 0, NF)
	for _, f := range Fields {
		names = append(names, f.Name)
	}
	sel := selectedCols(t.Fields).names
	writeJSON(w, 200, map[string]any{"ok": true, "enabled": t.Enabled, "intervalS": t.IntervalS, "fields": sel, "all": names})
	return nil
}

func (s *Server) trackingSet(w http.ResponseWriter, r *http.Request) error {
	var in Tracking
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.IntervalS < 1 || in.IntervalS > 3600 {
		return &HTTPError{400, "Interval must be between 1 and 3600 seconds."}
	}
	for _, n := range in.Fields {
		if _, ok := fieldIdx[n]; !ok {
			return &HTTPError{400, "Unknown field: " + n}
		}
	}
	if in.Enabled && len(in.Fields) == 0 {
		return &HTTPError{400, "Select at least one field to track."}
	}
	if err := s.cfg.Update(func(c *Config) { c.Tracking = in }); err != nil {
		return err
	}
	if err := s.logs.ApplyTracking(); err != nil {
		return err
	}
	return s.trackingGet(w, r)
}

// trackingTest fetches one fresh measurement and reports which known fields have data.
func (s *Server) trackingTest(w http.ResponseWriter, r *http.Request) error {
	m, at, err := s.meter.Measurement(r.Context(), 0)
	if err != nil {
		return err
	}
	smp := SampleFromMeasurement(m, at)
	type fv struct {
		Name  string `json:"name"`
		Value F      `json:"value"`
	}
	out := make([]fv, 0, NF)
	for i, f := range Fields {
		out = append(out, fv{f.Name, F(smp.V[i])})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "at": at.Unix(), "fields": out})
	return nil
}

func (s *Server) liveStatus(w http.ResponseWriter, r *http.Request) error {
	writeJSON(w, 200, s.logs.LiveStatus())
	return nil
}

func (s *Server) liveStart(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		IntervalS int `json:"intervalS"`
		Minutes   int `json:"minutes"`
	}
	if err := readJSON(r, &in); err != nil {
		return err
	}
	if in.IntervalS < 1 || in.IntervalS > 300 {
		return &HTTPError{400, "Live interval must be between 1 and 300 seconds."}
	}
	if in.Minutes < 1 || in.Minutes > 24*60 {
		return &HTTPError{400, "Maximum duration must be between 1 and 1440 minutes."}
	}
	_, err := s.logs.Create("Live "+time.Now().Format("2 Jan 15:04"), "Live tracking session", in.IntervalS, in.Minutes, true)
	if err != nil {
		return err
	}
	writeJSON(w, 200, s.logs.LiveStatus())
	return nil
}

func (s *Server) liveStop(w http.ResponseWriter, r *http.Request) error {
	if st := s.logs.LiveStatus(); st.Active {
		if err := s.logs.Stop(st.ID); err != nil {
			return err
		}
	}
	writeJSON(w, 200, s.logs.LiveStatus())
	return nil
}

// last returns the newest measurement we already have (no request to the meter).
func (s *Server) last(w http.ResponseWriter, r *http.Request) error {
	m, at := s.meter.Last()
	if m == nil {
		writeJSON(w, 200, map[string]any{"ok": true, "m": nil})
		return nil
	}
	writeJSON(w, 200, map[string]any{"ok": true, "m": m, "at": at.UnixMilli()})
	return nil
}

func serveCSV(w http.ResponseWriter, r *http.Request, path, name string) {
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(name), "-"), "-")
	if slug == "" {
		slug = "log"
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.csv"`, slug))
	http.ServeFile(w, r, path)
}

// ---------- routes ----------

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	files := http.FileServer(http.FS(sub))
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))

	mux.HandleFunc("GET /api/info", handle(s.info))
	mux.HandleFunc("POST /api/config", handle(s.saveHost))
	mux.HandleFunc("PUT /api/prices", handle(s.savePrices))
	mux.HandleFunc("PUT /api/labels", handle(s.saveLabels))
	mux.HandleFunc("POST /api/pair", handle(func(w http.ResponseWriter, r *http.Request) error {
		name, err := s.meter.Pair(r.Context())
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true, "name": name})
		return nil
	}))
	mux.HandleFunc("POST /api/forget", handle(func(w http.ResponseWriter, r *http.Request) error {
		// Only forgets locally; remove the user on the meter via the Users card.
		if err := s.cfg.Update(func(c *Config) { c.Token, c.UserName, c.PairedAt = "", "", "" }); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return nil
	}))

	mux.HandleFunc("GET /api/stream", s.stream)
	mux.HandleFunc("GET /api/panels", handle(func(w http.ResponseWriter, r *http.Request) error {
		p, custom := s.panels.Get()
		writeJSON(w, 200, map[string]any{"ok": true, "panels": p, "custom": custom})
		return nil
	}))
	mux.HandleFunc("PUT /api/panels", handle(func(w http.ResponseWriter, r *http.Request) error {
		var in struct {
			Panels []Panel `json:"panels"`
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<18)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return &HTTPError{400, "Invalid JSON body."}
		}
		p, err := s.panels.Set(in.Panels)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true, "panels": p, "custom": true})
		return nil
	}))
	mux.HandleFunc("POST /api/panels/reset", handle(func(w http.ResponseWriter, r *http.Request) error {
		p, err := s.panels.Reset()
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true, "panels": p, "custom": false})
		return nil
	}))
	mux.HandleFunc("GET /api/baseline", handle(func(w http.ResponseWriter, r *http.Request) error {
		writeJSON(w, 200, map[string]any{"ok": true, "baseline": s.base.Get()})
		return nil
	}))
	mux.HandleFunc("PUT /api/baseline", handle(func(w http.ResponseWriter, r *http.Request) error {
		var in Baseline
		r.Body = http.MaxBytesReader(w, r.Body, 1<<17)
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
			return &HTTPError{400, "Invalid JSON body."}
		}
		b, err := s.base.Set(in)
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true, "baseline": b})
		return nil
	}))
	mux.HandleFunc("GET /api/last", handle(s.last))
	mux.HandleFunc("GET /api/live/status", handle(s.liveStatus))
	mux.HandleFunc("POST /api/live/start", handle(s.liveStart))
	mux.HandleFunc("POST /api/live/stop", handle(s.liveStop))
	mux.HandleFunc("GET /api/tracking", handle(s.trackingGet))
	mux.HandleFunc("POST /api/tracking", handle(s.trackingSet))
	mux.HandleFunc("POST /api/tracking/test", handle(s.trackingTest))
	mux.HandleFunc("GET /api/live", handle(s.live))
	mux.HandleFunc("GET /api/history", handle(s.history))

	// device endpoints (explicit allowlist)
	mux.HandleFunc("GET /api/device", s.proxy("GET", "/api", ""))
	mux.HandleFunc("GET /api/measurement", s.proxy("GET", "/api/measurement", ""))
	mux.HandleFunc("GET /api/telegram", s.proxy("GET", "/api/telegram", "text/plain"))
	mux.HandleFunc("GET /api/system", s.proxy("GET", "/api/system", ""))
	mux.HandleFunc("PUT /api/system", handle(s.putSystem))
	mux.HandleFunc("POST /api/system/identify", s.proxy("PUT", "/api/system/identify", ""))
	mux.HandleFunc("POST /api/system/reboot", s.proxy("PUT", "/api/system/reboot", ""))
	mux.HandleFunc("GET /api/batteries", s.proxy("GET", "/api/batteries", ""))
	mux.HandleFunc("PUT /api/batteries", handle(s.putBatteries))
	mux.HandleFunc("GET /api/users", s.proxy("GET", "/api/user", ""))
	mux.HandleFunc("DELETE /api/users", handle(s.deleteUser))
	mux.HandleFunc("GET /api/all", handle(s.all))

	// logs
	mux.HandleFunc("GET /api/logs", handle(s.listLogs))
	mux.HandleFunc("POST /api/logs", handle(s.createLog))
	mux.HandleFunc("POST /api/logs/{id}/stop", handle(func(w http.ResponseWriter, r *http.Request) error {
		if err := s.logs.Stop(r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return nil
	}))
	mux.HandleFunc("POST /api/logs/{id}/resume", handle(func(w http.ResponseWriter, r *http.Request) error {
		if err := s.logs.Resume(r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return nil
	}))
	mux.HandleFunc("DELETE /api/logs/{id}", handle(func(w http.ResponseWriter, r *http.Request) error {
		if err := s.logs.Delete(r.PathValue("id")); err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return nil
	}))
	mux.HandleFunc("GET /api/logs/{id}/data", handle(s.logData))
	mux.HandleFunc("GET /api/logs/{id}/download", handle(func(w http.ResponseWriter, r *http.Request) error {
		p, name, err := s.logs.Path(r.PathValue("id"))
		if err != nil {
			return err
		}
		serveCSV(w, r, p, name)
		return nil
	}))
	mux.HandleFunc("GET /api/master/download", handle(func(w http.ResponseWriter, r *http.Request) error {
		d, err := time.ParseInLocation("2006-01-02", r.URL.Query().Get("date"), time.Local)
		if err != nil {
			return &HTTPError{400, "date must be YYYY-MM-DD"}
		}
		p := s.logs.master.DayPath(d)
		if _, err := os.Stat(p); err != nil {
			return &HTTPError{404, "No log for that day."}
		}
		serveCSV(w, r, p, "master-"+dayKey(d))
		return nil
	}))

	return s.middleware(mux)
}

// lanAddrs lists addresses the UI can be reached on, for the startup message.
func lanAddrs() []string {
	var out []string
	ifs, _ := net.Interfaces()
	for _, ifc := range ifs {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				out = append(out, ipn.IP.String())
			}
		}
	}
	return out
}
