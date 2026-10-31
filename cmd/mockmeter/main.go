// mockmeter pretends to be a HomeWizard P1 meter (API v2) for development:
//
//	go run ./cmd/mockmeter -listen :8443                # serve fake meter over HTTPS
//	go run ./cmd/mockmeter -gen ./data/logs/master -days 9   # write fake history
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"log"
	"math"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const token = "ABCDEF0123456789ABCDEF0123456789"

var fieldNames = "power_w,power_l1_w,power_l2_w,power_l3_w,voltage_l1_v,voltage_l2_v,voltage_l3_v,current_l1_a,current_l2_a,current_l3_a,frequency_hz,energy_import_kwh,energy_import_t1_kwh,energy_import_t2_kwh,energy_export_kwh,energy_export_t1_kwh,energy_export_t2_kwh,gas_m3,water_m3,tariff"

func noise(t int64, salt string) float64 { // deterministic in [-1,1)
	h := fnv.New64a()
	fmt.Fprintf(h, "%d/%s", t, salt)
	return float64(h.Sum64()%2000000)/1000000 - 1
}

// state integrates the fake house forward in 5 s steps.
type state struct {
	t                          int64
	impT1, impT2, expT1, expT2 float64
	gasTrue, gasRep            float64
	water                      float64
	// instantaneous
	power, l1, l2, l3, v1, v2, v3, hz float64
	tariff                            int
}

func hourF(t time.Time) float64 { return float64(t.Hour()) + float64(t.Minute())/60 }

func (s *state) step(unix int64) {
	t := time.Unix(unix, 0)
	h := hourF(t)
	day := unix / 86400
	// load: base + morning/evening peaks + appliance bursts
	load := 140 + 40*noise(unix/60, "base")
	load += 450*bump(h, 7.5, 1.0) + 900*bump(h, 18.5, 2.0) + 250*bump(h, 12.5, 1.0)
	// kettle / oven / washing machine at day-dependent times
	for i, ev := range []struct {
		at, dur float64
		w       float64
	}{{7.2, 0.08, 2000}, {18.3, 0.8, 2400}, {10.5 + 3*math.Abs(noise(day, "wm")), 1.5, 1800}, {20.5, 0.1, 2000}} {
		if h >= ev.at && h < ev.at+ev.dur {
			load += ev.w * (0.8 + 0.2*noise(unix/30, fmt.Sprint("ev", i)))
		}
	}
	load += 60 * noise(unix, "jitter")
	// small solar array (late October: low)
	sun := math.Max(0, math.Sin(math.Pi*(h-8)/9)) * (700 + 500*(noise(day, "sun")+1)/2)
	sun *= 0.6 + 0.4*(noise(unix/600, "cloud")+1)/2
	net := load - sun
	f1, f2 := 0.5+0.1*noise(unix/300, "f1"), 0.3
	s.l1, s.l2, s.l3 = net*f1, net*f2, net*(1-f1-f2)
	s.power = net
	s.v1, s.v2, s.v3 = 231+2*noise(unix/20, "v1"), 232+2*noise(unix/20, "v2"), 230+2*noise(unix/20, "v3")
	s.hz = 50 + 0.05*noise(unix, "hz")
	s.tariff = 1
	if wd := t.Weekday(); wd != time.Saturday && wd != time.Sunday && h >= 7 && h < 23 {
		s.tariff = 2
	}
	if s.t != 0 {
		dt := float64(unix-s.t) / 3600
		kwh := net / 1000 * dt
		switch {
		case net >= 0 && s.tariff == 1:
			s.impT1 += kwh
		case net >= 0:
			s.impT2 += kwh
		case s.tariff == 1:
			s.expT1 -= kwh
		default:
			s.expT2 -= kwh
		}
		gas := 0.04 + 0.9*bump(h, 7, 1.2) + 0.7*bump(h, 18.5, 2) + 0.25*bump(h, 12.3, 0.5)
		s.gasTrue += gas * dt * (0.9 + 0.1*noise(unix/300, "gas"))
		s.water += (0.002 + 0.15*bump(h, 7.3, 0.4) + 0.2*bump(h, 19, 0.7)) * dt
	}
	if s.t == 0 || unix/300 != s.t/300 { // gas meter reports every 5 minutes
		s.gasRep = s.gasTrue
	}
	s.t = unix
}

func bump(h, c, w float64) float64 { return math.Exp(-((h - c) * (h - c)) / (2 * w * w)) }

func newState() *state {
	return &state{impT1: 4210.5, impT2: 9568.8, expT1: 120.2, expT2: 345.7, gasTrue: 2569.6, gasRep: 2569.6, water: 123.4}
}

func (s *state) measurement() map[string]any {
	imp, exp := s.impT1+s.impT2, s.expT1+s.expT2
	r3 := func(v float64) float64 { return math.Round(v*1000) / 1000 }
	return map[string]any{
		"protocol_version": 50, "meter_model": "ISKRA  2M550T-101", "unique_id": "4E6576657220476F6E6E61",
		"timestamp": time.Unix(s.t, 0).Format("2006-01-02T15:04:05"), "tariff": s.tariff,
		"energy_import_kwh": r3(imp), "energy_import_t1_kwh": r3(s.impT1), "energy_import_t2_kwh": r3(s.impT2),
		"energy_export_kwh": r3(exp), "energy_export_t1_kwh": r3(s.expT1), "energy_export_t2_kwh": r3(s.expT2),
		"power_w": math.Round(s.power), "power_l1_w": math.Round(s.l1), "power_l2_w": math.Round(s.l2), "power_l3_w": math.Round(s.l3),
		"voltage_l1_v": r3(s.v1), "voltage_l2_v": r3(s.v2), "voltage_l3_v": r3(s.v3),
		"current_l1_a": r3(s.l1 / s.v1), "current_l2_a": r3(s.l2 / s.v2), "current_l3_a": r3(s.l3 / s.v3),
		"frequency_hz": r3(s.hz),
		"external": []any{
			map[string]any{"unique_id": "47", "type": "gas_meter", "timestamp": time.Unix(s.t/300*300, 0).Format("2006-01-02T15:04:05"), "value": r3(s.gasRep), "unit": "m3"},
			map[string]any{"unique_id": "48", "type": "water_meter", "timestamp": time.Unix(s.t, 0).Format("2006-01-02T15:04:05"), "value": r3(s.water), "unit": "m3"},
		},
	}
}

func (s *state) csv() string {
	m := s.measurement()
	var sb strings.Builder
	sb.WriteString(time.Unix(s.t, 0).Format(time.RFC3339))
	for _, n := range strings.Split(fieldNames, ",") {
		sb.WriteByte(',')
		switch n {
		case "gas_m3":
			fmt.Fprint(&sb, m["external"].([]any)[0].(map[string]any)["value"])
		case "water_m3":
			fmt.Fprint(&sb, m["external"].([]any)[1].(map[string]any)["value"])
		default:
			fmt.Fprint(&sb, m[n])
		}
	}
	sb.WriteByte('\n')
	return sb.String()
}

const simStart = "2026-09-25T00:00:00Z"

func gen(dir string, days int) {
	os.MkdirAll(dir, 0o755)
	st := newState()
	start, _ := time.Parse(time.RFC3339, simStart)
	now := time.Now()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location()).AddDate(0, 0, -days)
	var f *os.File
	var cur string
	for u := start.Unix(); u <= now.Unix(); u += 5 {
		st.step(u)
		if u < from.Unix() {
			continue
		}
		// pretend the Pi was off for a while two days ago
		if tm := time.Unix(u, 0); tm.After(now.AddDate(0, 0, -2).Add(-3*time.Hour)) && tm.Before(now.AddDate(0, 0, -2).Add(-2*time.Hour)) {
			continue
		}
		name := filepath.Join(dir, time.Unix(u, 0).Format("2006-01-02")+".csv")
		if name != cur {
			if f != nil {
				f.Close()
			}
			var err error
			f, err = os.Create(name)
			if err != nil {
				log.Fatal(err)
			}
			fmt.Fprintf(f, "timestamp,%s\n", fieldNames)
			cur = name
		}
		f.WriteString(st.csv())
	}
	if f != nil {
		f.Close()
	}
	log.Printf("wrote %d days to %s", days, dir)
}

func selfSigned() tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "appliance/p1dongle/5c2fafaabbcc"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour * 365)}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func main() {
	listen := flag.String("listen", ":8443", "listen address (TLS)")
	genDir := flag.String("gen", "", "write fake master-log history into this folder and exit")
	days := flag.Int("days", 9, "days of history for -gen")
	omit := flag.String("omit", "", "comma separated measurement fields the fake meter does not report (e.g. power_l2_w,water_meter)")
	flag.Parse()
	if *genDir != "" {
		gen(*genDir, *days)
		return
	}

	st := newState()
	start, _ := time.Parse(time.RFC3339, simStart)
	for u := start.Unix(); u <= time.Now().Unix(); u += 5 {
		st.step(u)
	}
	var mu sync.Mutex
	system := map[string]any{"wifi_ssid": "Mock WiFi", "wifi_rssi_db": -61, "cloud_enabled": true, "uptime_s": 1234, "status_led_brightness_pct": 100, "api_v1_enabled": true}
	bat := map[string]any{"mode": "zero", "permissions": []any{"charge_allowed", "discharge_allowed"}, "charge_to_full": false,
		"battery_count": 2, "power_w": -404, "target_power_w": -400, "max_consumption_w": 1600, "max_production_w": 800}
	users := []map[string]any{{"name": "local/p1-go-app", "current": true}, {"name": "cloud/cloud_user"}, {"name": "local/p1-deno-app"}}
	presses := 0
	var lastMeasure time.Time
	_ = lastMeasure

	js := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("X-Api-Version") != "2" {
			js(w, 400, map[string]any{"error": "request:api-version-not-supported"})
			return
		}
		log.Printf("%s %s", r.Method, r.URL.Path)
		if r.URL.Path == "/api/user" && r.Method == "POST" {
			presses++
			if presses < 3 {
				js(w, 403, map[string]any{"error": "user:creation-not-enabled"})
				return
			}
			js(w, 200, map[string]any{"name": "local/p1-go-app", "token": token})
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+token {
			js(w, 401, map[string]any{"error": "user:unauthorized"})
			return
		}
		var body map[string]any
		if r.Body != nil {
			json.NewDecoder(r.Body).Decode(&body)
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api":
			js(w, 200, map[string]any{"product_name": "P1 Meter", "product_type": "HWE-P1", "serial": "5c2fafaabbcc", "firmware_version": "6.00", "api_version": "2.3.0"})
		case "GET /api/measurement":
			st.step(time.Now().Unix())
			m := st.measurement()
			for _, k := range strings.Split(*omit, ",") {
				delete(m, k)
				if k == "water_meter" {
					m["external"] = m["external"].([]any)[:1]
				}
			}
			js(w, 200, m)
		case "GET /api/telegram":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "/ISK5\\2M550T-1011\r\n\r\n1-3:0.2.8(50)\r\n0-0:1.0.0(261031120000W)\r\n1-0:1.8.1(004210.500*kWh)\r\n1-0:1.8.2(009568.800*kWh)\r\n1-0:1.7.0(00.412*kW)\r\n!1F28\r\n")
		case "GET /api/system":
			system["uptime_s"] = system["uptime_s"].(int) + 1
			js(w, 200, system)
		case "PUT /api/system":
			for k, v := range body {
				system[k] = v
			}
			js(w, 200, system)
		case "PUT /api/system/identify", "PUT /api/system/reboot":
			w.WriteHeader(204)
		case "GET /api/batteries":
			js(w, 200, bat)
		case "PUT /api/batteries":
			for k, v := range body {
				bat[k] = v
			}
			js(w, 200, bat)
		case "GET /api/user":
			js(w, 200, users)
		case "DELETE /api/user":
			n, _ := body["name"].(string)
			for i, u := range users {
				if u["name"] == n {
					users = append(users[:i], users[i+1:]...)
					w.WriteHeader(204)
					return
				}
			}
			js(w, 404, map[string]any{"error": "user:not-found"})
		default:
			js(w, 404, map[string]any{"error": "request:unknown"})
		}
	})
	srv := &http.Server{Addr: *listen, Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{selfSigned()}}}
	log.Printf("mock P1 meter on https://localhost%s (token %s)", *listen, token)
	log.Fatal(srv.ListenAndServeTLS("", ""))
}
