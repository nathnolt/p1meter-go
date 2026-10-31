package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Prices are only used for the cost estimates in the UI.
type Prices struct {
	Currency string  `json:"currency"`
	Import   float64 `json:"import"` // per kWh
	Export   float64 `json:"export"` // per kWh fed back
	Gas      float64 `json:"gas"`    // per m3
}

// Tracking configures the always-on "global tracking" (the master log).
type Tracking struct {
	Enabled   bool     `json:"enabled"`
	IntervalS int      `json:"intervalS"`
	Fields    []string `json:"fields"` // CSV columns to record; empty = all
}

// Config is stored in <data>/config.json. The first four fields are the same
// as in the Deno version, so an existing config.json can be reused as-is.
type Config struct {
	Host     string   `json:"host,omitempty"`
	Token    string   `json:"token,omitempty"`
	UserName string   `json:"userName,omitempty"`
	PairedAt string   `json:"pairedAt,omitempty"`
	Prices   Prices   `json:"prices"`
	Tracking Tracking `json:"tracking"`
	// Labels renames fields in the UI (field id -> text); "tariff:1" / "tariff:2" name the tariffs.
	Labels map[string]string `json:"labels,omitempty"`
}

type ConfigStore struct {
	mu   sync.RWMutex
	path string
	cfg  Config
}

func NewConfigStore(path string) (*ConfigStore, error) {
	s := &ConfigStore{path: path}
	s.cfg.Prices = Prices{Currency: "€", Import: 0.30, Export: 0.08, Gas: 1.30}
	s.cfg.Tracking = Tracking{Enabled: false, IntervalS: 5}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(b, &s.cfg); err != nil {
		return nil, err
	}
	if s.cfg.Tracking.IntervalS < 1 || s.cfg.Tracking.IntervalS > 3600 {
		s.cfg.Tracking.IntervalS = 5
	}
	if s.cfg.Prices.Currency == "" {
		s.cfg.Prices.Currency = "€"
	}
	return s, nil
}

func (s *ConfigStore) Get() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Update applies fn and persists atomically (the token grants full access to
// the meter, so the file is chmod 0600).
func (s *ConfigStore) Update(fn func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}
