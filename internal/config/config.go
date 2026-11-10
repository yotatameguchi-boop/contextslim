// Package config loads, validates and saves ContextSlim settings.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// Modes.
const (
	ModeObserve  = "observe"
	ModeCompress = "compress"
)

// Token estimators.
const (
	// EstimatorNone shows byte counts only (default). See design 12.
	EstimatorNone = "none"
	// EstimatorBytesDiv4 is an unverified rough heuristic (bytes/4). It is never
	// presented as an exact Claude token count.
	EstimatorBytesDiv4 = "bytes_div_4"
)

// AllParsers lists every parser shipped in the MVP.
var AllParsers = []string{"vitest", "jest", "tsc", "pytest", "ruff"}

// Config is the persisted configuration (config.json in the data directory).
type Config struct {
	Mode                 string   `json:"mode"`
	EnabledParsers       []string `json:"enabled_parsers"`
	MinInputBytes        int      `json:"min_input_bytes"`
	MinSavingsBytes      int      `json:"min_savings_bytes"`
	MinSavingsRatio      float64  `json:"min_savings_ratio"`
	MaxInputBytes        int      `json:"max_input_bytes"`
	ProcessingBudgetMS   int      `json:"processing_budget_ms"`
	RetentionDays        int      `json:"retention_days"`
	MetricsRetentionDays int      `json:"metrics_retention_days"`
	MaxStorageBytes      int64    `json:"max_storage_bytes"`
	TokenEstimator       string   `json:"token_estimator"`
}

// Default returns the initial configuration from the design (section 10).
func Default() Config {
	return Config{
		Mode:                 ModeObserve,
		EnabledParsers:       append([]string(nil), AllParsers...),
		MinInputBytes:        4096,
		MinSavingsBytes:      1024,
		MinSavingsRatio:      0.10,
		MaxInputBytes:        10 << 20,
		ProcessingBudgetMS:   200,
		RetentionDays:        7,
		MetricsRetentionDays: 30,
		MaxStorageBytes:      500 << 20,
		TokenEstimator:       EstimatorNone,
	}
}

// Validate reports the first invalid setting.
func (c Config) Validate() error {
	switch c.Mode {
	case ModeObserve, ModeCompress:
	default:
		return fmt.Errorf("mode must be %q or %q, got %q", ModeObserve, ModeCompress, c.Mode)
	}
	known := map[string]bool{}
	for _, p := range AllParsers {
		known[p] = true
	}
	for _, p := range c.EnabledParsers {
		if !known[p] {
			return fmt.Errorf("enabled_parsers: unknown parser %q", p)
		}
	}
	switch {
	case c.MinInputBytes < 0:
		return errors.New("min_input_bytes must be >= 0")
	case c.MinSavingsBytes < 0:
		return errors.New("min_savings_bytes must be >= 0")
	case c.MinSavingsRatio < 0 || c.MinSavingsRatio >= 1:
		return errors.New("min_savings_ratio must be in [0, 1)")
	case c.MaxInputBytes <= 0:
		return errors.New("max_input_bytes must be > 0")
	case c.ProcessingBudgetMS <= 0 || c.ProcessingBudgetMS > 5000:
		return errors.New("processing_budget_ms must be in 1..5000 (keep it below the hook timeout)")
	case c.RetentionDays <= 0:
		return errors.New("retention_days must be > 0")
	case c.MetricsRetentionDays <= 0:
		return errors.New("metrics_retention_days must be > 0")
	case c.MaxStorageBytes <= 0:
		return errors.New("max_storage_bytes must be > 0")
	}
	switch c.TokenEstimator {
	case EstimatorNone, EstimatorBytesDiv4:
	default:
		return fmt.Errorf("token_estimator must be %q or %q", EstimatorNone, EstimatorBytesDiv4)
	}
	return nil
}

// ParserEnabled reports whether the named parser is enabled.
func (c Config) ParserEnabled(name string) bool {
	for _, p := range c.EnabledParsers {
		if p == name {
			return true
		}
	}
	return false
}

// Path returns the config file path inside dataDir.
func Path(dataDir string) string { return filepath.Join(dataDir, "config.json") }

// Load reads config.json. A missing file yields Default(). Fields absent from
// the file keep their default values. Unknown fields are rejected so typos are
// reported instead of silently ignored.
func Load(dataDir string) (Config, error) {
	c := Default()
	b, err := os.ReadFile(Path(dataDir))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Default(), fmt.Errorf("%s: %w", Path(dataDir), err)
	}
	if err := c.Validate(); err != nil {
		return Default(), fmt.Errorf("%s: %w", Path(dataDir), err)
	}
	sort.Strings(c.EnabledParsers)
	return c, nil
}

// Save writes config.json atomically with user-only permissions.
func Save(dataDir string, c Config) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dataDir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil && !isUnsupported(err) {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, Path(dataDir))
}

func isUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }
