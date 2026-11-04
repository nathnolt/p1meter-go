package main

import (
	"encoding/json"
	"math"
	"os"
	"strings"
	"sync"
)

// Baseline = devices that are always on. Their summed wattage is deducted from the power charts in the browser
// so you can see what everything else is doing. Only the list is stored here (data/baseline.json).
type BaselineItem struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Watts   float64 `json:"watts"`
	Enabled bool    `json:"enabled"`
	Phase   string  `json:"phase"` // "" = spread evenly over L1–L3, or "1", "2", "3"
}

type Baseline struct {
	Enabled bool           `json:"enabled"`
	Items   []BaselineItem `json:"items"`
}

type BaselineStore struct {
	mu   sync.Mutex
	path string
}

func NewBaselineStore(path string) *BaselineStore { return &BaselineStore{path: path} }

func (bs *BaselineStore) Get() Baseline {
	bs.mu.Lock()
	defer bs.mu.Unlock()
	var b Baseline
	if raw, err := os.ReadFile(bs.path); err == nil {
		_ = json.Unmarshal(raw, &b)
	}
	if b.Items == nil {
		b.Items = []BaselineItem{}
	}
	return b
}

func cleanBaseline(in Baseline) (Baseline, error) {
	if len(in.Items) > 100 {
		return in, &HTTPError{400, "At most 100 devices."}
	}
	out := Baseline{Enabled: in.Enabled, Items: make([]BaselineItem, 0, len(in.Items))}
	seen := map[string]bool{}
	for i, it := range in.Items {
		it.Name = strings.TrimSpace(it.Name)
		if len(it.Name) > 60 {
			it.Name = it.Name[:60]
		}
		if math.IsNaN(it.Watts) || math.IsInf(it.Watts, 0) || it.Watts < 0 || it.Watts > 100000 {
			return in, &HTTPError{400, "Wattage must be a number between 0 and 100000."}
		}
		if it.Phase != "1" && it.Phase != "2" && it.Phase != "3" {
			it.Phase = ""
		}
		it.Watts = math.Round(it.Watts*10) / 10
		if it.ID == "" || seen[it.ID] || len(it.ID) > 40 {
			it.ID = "d" + string(rune('a'+i%26)) + itoa(i)
		}
		seen[it.ID] = true
		out.Items = append(out.Items, it)
	}
	return out, nil
}

func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }

func (bs *BaselineStore) Set(in Baseline) (Baseline, error) {
	b, err := cleanBaseline(in)
	if err != nil {
		return b, err
	}
	raw, _ := json.MarshalIndent(b, "", "  ")
	bs.mu.Lock()
	defer bs.mu.Unlock()
	tmp := bs.path + ".tmp"
	if err := os.MkdirAll(dirOf(bs.path), 0o755); err != nil {
		return b, err
	}
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return b, err
	}
	return b, os.Rename(tmp, bs.path)
}

func dirOf(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[:i]
	}
	return "."
}
