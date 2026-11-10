// Package store saves original tool outputs and manages retention.
//
// Layout (inside the data directory):
//
//	config.json
//	raw/<session-key>/<raw-id>.json    original output + metadata
//	raw/<session-key>/<raw-id>.gone    tombstone left when a raw file is pruned
//	metrics/<session-key>.jsonl        per-session events
//
// Every path is built from validated, generated names. Nothing taken from hook
// input is used as a path component as-is.
package store

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Errors.
var (
	ErrInvalidID = errors.New("invalid raw id")
	ErrNotFound  = errors.New("raw output not found")
	ErrDeleted   = errors.New("raw output was deleted")
	ErrCorrupt   = errors.New("raw output failed its integrity check")
	ErrNoSpace   = errors.New("storage limit reached")
)

var (
	idPattern         = regexp.MustCompile(`^cs_[0-9a-f]{20}$`)
	sessionKeyPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// ProtectAge is how long a freshly written raw file is protected from
// capacity-based pruning, so a reference Claude just received stays valid.
const ProtectAge = 10 * time.Minute

// DataDir returns the ContextSlim data directory. CONTEXTSLIM_HOME overrides it.
func DataDir() (string, error) {
	if d := os.Getenv("CONTEXTSLIM_HOME"); d != "" {
		return filepath.Abs(d)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "ContextSlim"), nil
	case "windows":
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "ContextSlim"), nil
		}
		return filepath.Join(home, "AppData", "Local", "ContextSlim"), nil
	default:
		if d := os.Getenv("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
			return filepath.Join(d, "contextslim"), nil
		}
		return filepath.Join(home, ".local", "share", "contextslim"), nil
	}
}

// SessionKey derives a path-safe key from a Claude Code session id.
func SessionKey(sessionID string) string {
	if sessionID == "" {
		sessionID = "no-session"
	}
	sum := sha256.Sum256([]byte("contextslim-session:" + sessionID))
	return hex.EncodeToString(sum[:8])
}

// ValidID reports whether id has the raw-id format.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// Record is the content of a raw file.
type Record struct {
	FormatVersion int    `json:"format_version"`
	RawID         string `json:"raw_id"`
	SessionKey    string `json:"session_key"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	ToolName      string `json:"tool_name"`
	Command       string `json:"command"`
	Parser        string `json:"parser"`
	Compressor    string `json:"compressor"`
	// Field is the tool_response field that was compressed ("stdout"/"stderr").
	Field string `json:"field"`
	// SHA256 covers ToolResponse exactly as stored.
	SHA256       string          `json:"sha256"`
	ToolResponse json.RawMessage `json:"tool_response"`
}

// Store manages the data directory.
type Store struct {
	Dir string
	Now func() time.Time
}

// New returns a Store rooted at dir.
func New(dir string) *Store { return &Store{Dir: dir, Now: time.Now} }

func (s *Store) rawDir() string     { return filepath.Join(s.Dir, "raw") }
func (s *Store) metricsDir() string { return filepath.Join(s.Dir, "metrics") }

// MetricsDir is where per-session JSONL files live.
func (s *Store) MetricsDir() string { return s.metricsDir() }

// NewID returns a new random raw id.
func NewID() (string, error) {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "cs_" + hex.EncodeToString(b[:]), nil
}

// SaveRaw stores the original tool response and returns the new raw id.
// It writes to a temporary file and renames it into place; a raw id is never
// reused (O_EXCL on the final name is checked before the rename).
func (s *Store) SaveRaw(rec Record, retention time.Duration) (string, error) {
	if !sessionKeyPattern.MatchString(rec.SessionKey) {
		return "", fmt.Errorf("invalid session key")
	}
	dir := filepath.Join(s.rawDir(), rec.SessionKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	now := s.Now().UTC()
	// Store the response compacted but otherwise byte-identical (no HTML
	// escaping), and hash exactly the bytes that are written.
	var compact bytes.Buffer
	if err := json.Compact(&compact, rec.ToolResponse); err != nil {
		return "", err
	}
	rec.ToolResponse = compact.Bytes()
	sum := sha256.Sum256(rec.ToolResponse)
	rec.FormatVersion = 1
	rec.SHA256 = hex.EncodeToString(sum[:])
	rec.CreatedAt = now.Format(time.RFC3339Nano)
	rec.ExpiresAt = now.Add(retention).Format(time.RFC3339Nano)
	for attempt := 0; attempt < 5; attempt++ {
		id, err := NewID()
		if err != nil {
			return "", err
		}
		rec.RawID = id
		var enc bytes.Buffer
		e := json.NewEncoder(&enc)
		e.SetEscapeHTML(false)
		if err := e.Encode(rec); err != nil {
			return "", err
		}
		b := enc.Bytes()
		final := filepath.Join(dir, id+".json")
		// Reserve the name so concurrent writers cannot collide.
		f, err := os.OpenFile(final, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, fs.ErrExist) {
			continue
		}
		if err != nil {
			return "", err
		}
		f.Close()
		if err := writeAtomic(dir, final, b); err != nil {
			os.Remove(final)
			return "", err
		}
		return id, nil
	}
	return "", errors.New("could not allocate a unique raw id")
}

func writeAtomic(dir, final string, b []byte) error {
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, 0o600); err != nil && runtime.GOOS != "windows" {
		return err
	}
	if err := os.Rename(name, final); err != nil {
		return err
	}
	ok = true
	return nil
}

// Load returns the record for id. It never returns a different raw output:
// deleted records yield ErrDeleted, unknown ids ErrNotFound and a hash
// mismatch ErrCorrupt.
func (s *Store) Load(id string) (*Record, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}
	sessions, err := os.ReadDir(s.rawDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	for _, d := range sessions {
		if !d.IsDir() || !sessionKeyPattern.MatchString(d.Name()) {
			continue
		}
		base := filepath.Join(s.rawDir(), d.Name(), id)
		if b, err := readRegular(base + ".json"); err == nil {
			var rec Record
			if err := json.Unmarshal(b, &rec); err != nil || rec.RawID != id {
				return nil, ErrCorrupt
			}
			sum := sha256.Sum256(rec.ToolResponse)
			if hex.EncodeToString(sum[:]) != rec.SHA256 {
				return nil, ErrCorrupt
			}
			return &rec, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if b, err := readRegular(base + ".gone"); err == nil {
			return nil, fmt.Errorf("%w (%s)", ErrDeleted, strings.TrimSpace(string(b)))
		}
	}
	return nil, ErrNotFound
}

// readRegular reads a regular file and refuses symlinks or other file types,
// so nothing outside the managed area is followed.
func readRegular(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	return os.ReadFile(path)
}

// SessionKeyOf returns the session key of a stored raw id (for metrics).
func (s *Store) SessionKeyOf(id string) string {
	if !ValidID(id) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(s.rawDir(), "*", id+".*"))
	for _, m := range matches {
		k := filepath.Base(filepath.Dir(m))
		if sessionKeyPattern.MatchString(k) {
			return k
		}
	}
	return ""
}

// file is one managed file considered by pruning.
type file struct {
	path  string
	size  int64
	mtime time.Time
	kind  string // raw, tomb, metrics, tmp
}

func (s *Store) list() ([]file, error) {
	var out []file
	add := func(root string, depth int) error {
		return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil // never follow or delete symlinks
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			name := d.Name()
			var kind string
			switch {
			case strings.HasPrefix(name, ".tmp-"):
				kind = "tmp"
			case strings.HasSuffix(name, ".json") && ValidID(strings.TrimSuffix(name, ".json")):
				kind = "raw"
			case strings.HasSuffix(name, ".gone") && ValidID(strings.TrimSuffix(name, ".gone")):
				kind = "tomb"
			case strings.HasSuffix(name, ".jsonl") && sessionKeyPattern.MatchString(strings.TrimSuffix(name, ".jsonl")):
				kind = "metrics"
			default:
				return nil // not ours: leave it alone
			}
			out = append(out, file{path: p, size: fi.Size(), mtime: fi.ModTime(), kind: kind})
			return nil
		})
	}
	if err := add(s.rawDir(), 2); err != nil {
		return nil, err
	}
	if err := add(s.metricsDir(), 1); err != nil {
		return nil, err
	}
	return out, nil
}

// Usage returns the bytes used by managed files.
func (s *Store) Usage() (int64, error) {
	files, err := s.list()
	if err != nil {
		return 0, err
	}
	var n int64
	for _, f := range files {
		n += f.size
	}
	return n, nil
}

// PruneOptions controls Prune.
type PruneOptions struct {
	Retention        time.Duration
	MetricsRetention time.Duration
	MaxBytes         int64
	// Reserve is extra space that must be available after pruning.
	Reserve int64
	DryRun  bool
}

// PruneResult summarizes a prune run.
type PruneResult struct {
	RawDeleted     int
	MetricsDeleted int
	TempDeleted    int
	BytesFreed     int64
	UsageAfter     int64
}

// Prune deletes expired files, then the oldest raw/metrics files until usage
// plus Reserve fits MaxBytes. Files younger than ProtectAge are never deleted
// for capacity reasons. Only files recognized as ContextSlim's own are touched.
func (s *Store) Prune(o PruneOptions) (PruneResult, error) {
	var r PruneResult
	files, err := s.list()
	if err != nil {
		return r, err
	}
	now := s.Now()
	var usage int64
	for _, f := range files {
		usage += f.size
	}
	var keep []file
	for _, f := range files {
		age := now.Sub(f.mtime)
		expired := false
		switch f.kind {
		case "tmp":
			expired = age > time.Hour
		case "raw":
			expired = age > o.Retention
		case "tomb":
			expired = age > o.Retention+30*24*time.Hour
		case "metrics":
			expired = age > o.MetricsRetention
		}
		if expired {
			if s.remove(f, "expired", o.DryRun, &r) {
				usage -= f.size
			}
			continue
		}
		keep = append(keep, f)
	}
	if o.MaxBytes > 0 && usage+o.Reserve > o.MaxBytes {
		sort.Slice(keep, func(i, j int) bool { return keep[i].mtime.Before(keep[j].mtime) })
		for _, f := range keep {
			if usage+o.Reserve <= o.MaxBytes {
				break
			}
			if f.kind == "tomb" || now.Sub(f.mtime) < ProtectAge {
				continue
			}
			if s.remove(f, "storage limit", o.DryRun, &r) {
				usage -= f.size
			}
		}
	}
	if !o.DryRun {
		s.removeEmptyDirs()
	}
	r.UsageAfter = usage
	if o.MaxBytes > 0 && usage+o.Reserve > o.MaxBytes {
		return r, ErrNoSpace
	}
	return r, nil
}

func (s *Store) remove(f file, reason string, dry bool, r *PruneResult) bool {
	if !s.inside(f.path) {
		return false
	}
	if !dry {
		if err := os.Remove(f.path); err != nil {
			return false
		}
		if f.kind == "raw" {
			tomb := strings.TrimSuffix(f.path, ".json") + ".gone"
			msg := fmt.Sprintf("deleted %s: %s", s.Now().UTC().Format(time.RFC3339), reason)
			_ = os.WriteFile(tomb, []byte(msg+"\n"), 0o600)
		}
	}
	switch f.kind {
	case "raw":
		r.RawDeleted++
	case "metrics":
		r.MetricsDeleted++
	case "tmp":
		r.TempDeleted++
	}
	r.BytesFreed += f.size
	return true
}

// inside reports whether p is strictly inside the data directory.
func (s *Store) inside(p string) bool {
	rel, err := filepath.Rel(s.Dir, p)
	if err != nil {
		return false
	}
	return rel != "." && !strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel)
}

func (s *Store) removeEmptyDirs() {
	entries, err := os.ReadDir(s.rawDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && sessionKeyPattern.MatchString(e.Name()) {
			_ = os.Remove(filepath.Join(s.rawDir(), e.Name())) // fails if not empty
		}
	}
}

// PruneStampPath is touched after each automatic prune.
func (s *Store) PruneStampPath() string { return filepath.Join(s.Dir, ".last_prune") }

// AutoPruneDue reports whether the last automatic prune is older than every.
func (s *Store) AutoPruneDue(every time.Duration) bool {
	fi, err := os.Stat(s.PruneStampPath())
	if err != nil {
		return true
	}
	return s.Now().Sub(fi.ModTime()) > every
}

// TouchPruneStamp records an automatic prune.
func (s *Store) TouchPruneStamp() {
	_ = os.MkdirAll(s.Dir, 0o700)
	_ = os.WriteFile(s.PruneStampPath(), []byte(s.Now().UTC().Format(time.RFC3339)+"\n"), 0o600)
	now := s.Now()
	_ = os.Chtimes(s.PruneStampPath(), now, now)
}
