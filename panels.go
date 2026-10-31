package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Panel is one user-defined chart on the History and log pages.
// Types: line | area | step | band (line with lowest…highest band) | bar | stacked | pie | donut.
type Panel struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Type   string   `json:"type"`   // line | area | step | bar | stacked
	Fields []string `json:"fields"` // field ids, see allowedPanelField
	Scale  string   `json:"scale"`  // auto | normalized
	Band   bool     `json:"band"`   // legacy: same as Type "band"
	Zero   string   `json:"zero"`   // auto | yes | no: does the value axis start at zero?
	Width  string   `json:"width"`  // half | full
	Height string   `json:"height"` // small | medium | large
	// Labels renames fields inside this panel only (field id -> text)
	Labels map[string]string `json:"labels,omitempty"`
}

var usageFields = []string{"import", "export", "import_t1", "import_t2", "gas", "water"}

// allowedPanelField: measurement ("power_w"), usage per period ("use:gas") or meter reading ("rd:gas_m3").
func allowedPanelField(id string) bool {
	switch {
	case strings.HasPrefix(id, "use:"):
		for _, u := range usageFields {
			if id == "use:"+u {
				return true
			}
		}
	case strings.HasPrefix(id, "rd:"):
		i, ok := fieldIdx[id[3:]]
		return ok && Fields[i].Kind != Gauge
	default:
		i, ok := fieldIdx[id]
		return ok && Fields[i].Kind == Gauge
	}
	return false
}

func defaultPanels() []Panel {
	return []Panel{
		{ID: "power", Name: "Power", Type: "band", Fields: []string{"power_w"}, Scale: "auto", Width: "full", Height: "large"},
		{ID: "electricity", Name: "Electricity", Type: "stacked", Fields: []string{"use:import", "use:export"}, Scale: "auto", Width: "half", Height: "medium"},
		{ID: "gas", Name: "Gas", Type: "bar", Fields: []string{"use:gas"}, Scale: "auto", Width: "half", Height: "medium"},
		{ID: "current", Name: "Phase current", Type: "line", Fields: []string{"current_l1_a", "current_l2_a", "current_l3_a"}, Scale: "auto", Width: "full", Height: "medium"},
	}
}

type PanelStore struct {
	mu   sync.Mutex
	path string
}

func NewPanelStore(path string) *PanelStore { return &PanelStore{path: path} }

// Get returns the saved panels, or the defaults when none were saved yet.
func (ps *PanelStore) Get() ([]Panel, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	b, err := os.ReadFile(ps.path)
	if err != nil {
		return defaultPanels(), false
	}
	var p []Panel
	if json.Unmarshal(b, &p) != nil || len(p) == 0 {
		return defaultPanels(), false
	}
	return p, true
}

func pick(v string, allowed ...string) string {
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	return allowed[0]
}

func cleanPanels(in []Panel) ([]Panel, error) {
	if len(in) > 40 {
		return nil, &HTTPError{400, "At most 40 panels."}
	}
	seen := map[string]bool{}
	out := make([]Panel, 0, len(in))
	for _, p := range in {
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			p.Name = "Panel"
		}
		if len(p.Name) > 60 {
			p.Name = p.Name[:60]
		}
		p.Type = pick(p.Type, "line", "area", "step", "band", "bar", "stacked", "pie", "donut")
		p.Zero = pick(p.Zero, "auto", "yes", "no")
		p.Scale = pick(p.Scale, "auto", "normalized")
		p.Width = pick(p.Width, "half", "full")
		p.Height = pick(p.Height, "medium", "small", "large")
		if len(p.Fields) == 0 || len(p.Fields) > 8 {
			return nil, &HTTPError{400, "Panel “" + p.Name + "” needs between 1 and 8 fields."}
		}
		dup := map[string]bool{}
		for _, f := range p.Fields {
			if !allowedPanelField(f) || dup[f] {
				return nil, &HTTPError{400, "Unknown or repeated field: " + f}
			}
			dup[f] = true
		}
		if len(p.ID) == 0 || len(p.ID) > 16 || seen[p.ID] || strings.Trim(p.ID, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
			b := make([]byte, 4)
			rand.Read(b)
			p.ID = hex.EncodeToString(b)
		}
		for k, v := range p.Labels {
			v = strings.TrimSpace(v)
			if !dup[k] || len(v) > 40 {
				delete(p.Labels, k)
			} else if v == "" {
				delete(p.Labels, k)
			} else {
				p.Labels[k] = v
			}
		}
		seen[p.ID] = true
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, &HTTPError{400, "Keep at least one panel (or use “Reset to defaults”)."}
	}
	return out, nil
}

func (ps *PanelStore) Set(in []Panel) ([]Panel, error) {
	p, err := cleanPanels(in)
	if err != nil {
		return nil, err
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	b, _ := json.MarshalIndent(p, "", "  ")
	if err := os.MkdirAll(filepath.Dir(ps.path), 0o755); err != nil {
		return nil, err
	}
	tmp := ps.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return nil, err
	}
	return p, os.Rename(tmp, ps.path)
}

func (ps *PanelStore) Reset() ([]Panel, error) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if err := os.Remove(ps.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return defaultPanels(), nil
}
