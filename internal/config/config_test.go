package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadSave(t *testing.T) {
	dir := t.TempDir()
	c, err := Load(dir)
	if err != nil || c.Mode != ModeObserve || c.MinInputBytes != 4096 || c.TokenEstimator != EstimatorNone {
		t.Fatalf("%+v %v", c, err)
	}
	c.Mode = ModeCompress
	if err := Save(dir, c); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(dir)
	if err != nil || c2.Mode != ModeCompress {
		t.Fatalf("%+v %v", c2, err)
	}
	// Partial file keeps defaults for missing fields.
	os.WriteFile(Path(dir), []byte(`{"mode":"compress","retention_days":3}`), 0o600)
	c3, err := Load(dir)
	if err != nil || c3.RetentionDays != 3 || c3.MaxInputBytes != 10<<20 {
		t.Fatalf("%+v %v", c3, err)
	}
	for _, bad := range []string{`{"mode":"x"}`, `{"typo_field":1}`, `{"enabled_parsers":["go"]}`, `{"min_savings_ratio":1.5}`, `{"token_estimator":"tiktoken"}`, `not json`} {
		os.WriteFile(Path(dir), []byte(bad), 0o600)
		if _, err := Load(dir); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if err := Save(filepath.Join(dir, "x"), Config{Mode: "bad"}); err == nil {
		t.Error("saved invalid config")
	}
}
