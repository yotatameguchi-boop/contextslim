package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func rec(session string) Record {
	return Record{SessionKey: SessionKey(session), ToolName: "Bash", Field: "stdout",
		ToolResponse: json.RawMessage(`{"stdout":"hello\n","stderr":"","interrupted":false}`)}
}

func TestSaveLoad(t *testing.T) {
	s := New(t.TempDir())
	id, err := s.SaveRaw(rec("a"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !ValidID(id) {
		t.Fatal(id)
	}
	r, err := s.Load(id)
	if err != nil || string(r.ToolResponse) != string(rec("a").ToolResponse) {
		t.Fatalf("%v %s", err, r.ToolResponse)
	}
	if s.SessionKeyOf(id) != SessionKey("a") {
		t.Error("session key")
	}
	fi, _ := os.Stat(filepath.Join(s.Dir, "raw", SessionKey("a"), id+".json"))
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("perm %v", fi.Mode().Perm())
	}
}

func TestLoadErrors(t *testing.T) {
	s := New(t.TempDir())
	for _, bad := range []string{"", "../etc/passwd", "cs_../../x", "cs_ZZZZZZZZZZZZZZZZZZZZ", "cs_0123"} {
		if _, err := s.Load(bad); !errors.Is(err, ErrInvalidID) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, err := s.Load("cs_0123456789abcdef0123"); !errors.Is(err, ErrNotFound) {
		t.Error(err)
	}
	id, _ := s.SaveRaw(rec("a"), time.Hour)
	p := filepath.Join(s.Dir, "raw", SessionKey("a"), id+".json")
	b, _ := os.ReadFile(p)
	var r Record
	json.Unmarshal(b, &r)
	r.ToolResponse = json.RawMessage(`{"stdout":"tampered","stderr":"","interrupted":false}`)
	b, _ = json.Marshal(r)
	os.WriteFile(p, b, 0o600)
	if _, err := s.Load(id); !errors.Is(err, ErrCorrupt) {
		t.Errorf("tampered: %v", err)
	}
}

func TestPruneExpiredLeavesTombstone(t *testing.T) {
	s := New(t.TempDir())
	id, _ := s.SaveRaw(rec("a"), time.Hour)
	foreign := filepath.Join(s.Dir, "raw", "notes.txt")
	os.WriteFile(foreign, []byte("keep me"), 0o600)
	old := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(filepath.Join(s.Dir, "raw", SessionKey("a"), id+".json"), old, old)
	r, err := s.Prune(PruneOptions{Retention: 7 * 24 * time.Hour, MetricsRetention: time.Hour, MaxBytes: 1 << 30})
	if err != nil || r.RawDeleted != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := s.Load(id); !errors.Is(err, ErrDeleted) {
		t.Fatalf("want ErrDeleted, got %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Error("prune touched a file it does not own")
	}
}

func TestPruneCapacityProtectsFresh(t *testing.T) {
	s := New(t.TempDir())
	var ids []string
	for i := 0; i < 3; i++ {
		id, _ := s.SaveRaw(rec("a"), time.Hour)
		ids = append(ids, id)
	}
	old := time.Now().Add(-time.Hour)
	os.Chtimes(filepath.Join(s.Dir, "raw", SessionKey("a"), ids[0]+".json"), old, old)
	// Limit fits only ~2 files: the old one goes, fresh ones stay.
	u, _ := s.Usage()
	r, err := s.Prune(PruneOptions{Retention: 24 * time.Hour, MetricsRetention: 24 * time.Hour, MaxBytes: u - 1})
	if err != nil || r.RawDeleted != 1 {
		t.Fatalf("%v %+v", err, r)
	}
	if _, err := s.Load(ids[0]); !errors.Is(err, ErrDeleted) {
		t.Error(err)
	}
	// Impossible limit: fresh files are protected, ErrNoSpace reported.
	if _, err := s.Prune(PruneOptions{Retention: 24 * time.Hour, MetricsRetention: 24 * time.Hour, MaxBytes: 1}); !errors.Is(err, ErrNoSpace) {
		t.Errorf("want ErrNoSpace, got %v", err)
	}
	for _, id := range ids[1:] {
		if _, err := s.Load(id); err != nil {
			t.Errorf("fresh %s deleted: %v", id, err)
		}
	}
}

func TestConcurrentSessions(t *testing.T) {
	s := New(t.TempDir())
	var wg sync.WaitGroup
	ids := make([]string, 40)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := rec(string(rune('a' + i%4)))
			r.ToolResponse = json.RawMessage(`{"stdout":"` + string(rune('A'+i%26)) + `","stderr":"","interrupted":false}`)
			id, err := s.SaveRaw(r, time.Hour)
			if err != nil {
				t.Error(err)
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, id := range ids {
		if seen[id] {
			t.Fatal("duplicate id")
		}
		seen[id] = true
		r, err := s.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"stdout":"` + string(rune('A'+i%26)) + `","stderr":"","interrupted":false}`
		if string(r.ToolResponse) != want || r.SessionKey != SessionKey(string(rune('a'+i%4))) {
			t.Fatalf("mixed up record %d: %s", i, r.ToolResponse)
		}
	}
}

// Claude Code sends <, > and & literally; they must survive storage and the
// integrity check (regression: json.Marshal HTML-escaped them).
func TestLiteralHTMLCharactersRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	r := rec("a")
	r.ToolResponse = json.RawMessage(`{"stdout":"pricing > total <b> & \u2028 日本語\n", "stderr":"","interrupted":false}`)
	id, err := s.SaveRaw(r, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	var v struct{ Stdout string }
	json.Unmarshal(got.ToolResponse, &v)
	if v.Stdout != "pricing > total <b> & \u2028 日本語\n" {
		t.Fatalf("%q", v.Stdout)
	}
}
