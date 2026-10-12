// Package session persists conversations as JSONL, one entry per line with
// parent links. hal keeps the tree but only uses the linear active branch.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rushera/hal/internal/agent"
	"github.com/Rushera/hal/internal/config"
)

// RootWith returns the canonical session storage root: an explicit directory
// (--session-dir or the config file), then $HAL_SESSION_DIR, then the XDG
// data directory. New sessions are always written here.
func RootWith(dir string) string {
	if strings.TrimSpace(dir) != "" {
		return config.ExpandPath(dir)
	}
	if v := strings.TrimSpace(os.Getenv("HAL_SESSION_DIR")); v != "" {
		return config.ExpandPath(v)
	}
	return config.Resolve().SessionDir
}

// SearchRoots returns the directories to search when resuming: the canonical
// root first, then the pre-XDG ~/.hal/sessions location when it still holds
// sessions, so earlier work stays reachable.
func SearchRoots(explicit string) []string {
	primary := RootWith(explicit)
	roots := []string{primary}
	legacy := config.Resolve().LegacySessionDir
	if legacy != primary {
		if st, err := os.Stat(legacy); err == nil && st.IsDir() {
			roots = append(roots, legacy)
		}
	}
	return roots
}

// LatestIn returns the most recent session across the given roots.
func LatestIn(roots []string, cwd string) (string, error) {
	var newest string
	var newestTime time.Time
	for _, root := range roots {
		infos, err := List(root, cwd)
		if err != nil || len(infos) == 0 {
			continue
		}
		// List is newest-first, but accumulate across roots to compare.
		for _, info := range infos {
			if newest == "" || info.UpdatedAt.After(newestTime) {
				newest, newestTime = info.Path, info.UpdatedAt
			}
		}
	}
	if newest == "" {
		return "", ErrNoSessions
	}
	return newest, nil
}

// ProjectDir is the per-working-directory bucket, derived from an absolute cwd
// so sessions are grouped by project.
func ProjectDir(root, cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	slug := strings.Trim(strings.ReplaceAll(filepath.ToSlash(abs), "/", "-"), "-")
	if slug == "" {
		slug = "root"
	}
	return filepath.Join(root, slug)
}

// Entry is one line of a session file.
type Entry struct {
	Type      string         `json:"type"` // session | message | model_change | name
	ID        string         `json:"id"`
	ParentID  string         `json:"parentId,omitempty"`
	Timestamp string         `json:"timestamp"`
	SessionID string         `json:"sessionId,omitempty"`
	Cwd       string         `json:"cwd,omitempty"`
	Model     string         `json:"model,omitempty"`
	Name      string         `json:"name,omitempty"`
	Message   *agent.Message `json:"message,omitempty"`
}

// Info summarises a stored session.
type Info struct {
	Path      string
	ID        string
	Name      string
	Cwd       string
	Model     string
	Messages  int
	UpdatedAt time.Time
}

// Session is an append-only conversation log.
type Session struct {
	ID       string
	Name     string
	Cwd      string
	Model    string
	Path     string
	Messages []agent.Message
	// UpdatedAt is the timestamp of the newest entry, used to order sessions.
	UpdatedAt time.Time

	mu     sync.Mutex
	lastID string
	file   *os.File
}

var (
	// ErrNoSessions is returned when a project has no stored sessions.
	ErrNoSessions = fmt.Errorf("no saved sessions for this directory")
)

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// New creates and persists a fresh session in the given root directory.
func New(root, cwd, name, model string) (*Session, error) {
	dir := ProjectDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	abs, _ := filepath.Abs(cwd)
	id := newID()
	stamp := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("%s-%s.jsonl", stamp, id[:8]))

	s := &Session{ID: id, Name: name, Cwd: abs, Model: model, Path: path}
	if err := s.open(); err != nil {
		return nil, err
	}
	header := Entry{Type: "session", ID: newID(), Timestamp: now(), SessionID: id, Cwd: abs, Model: model, Name: name}
	if err := s.write(header); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Session) open() error {
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	s.file = f
	return nil
}

func (s *Session) write(e Entry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.ID == "" {
		e.ID = newID()
	}
	if e.ParentID == "" {
		e.ParentID = s.lastID
	}
	if e.Timestamp == "" {
		e.Timestamp = now()
	}
	if s.file == nil {
		if err := s.open(); err != nil {
			return err
		}
	}
	data, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := s.file.Write(append(data, '\n')); err != nil {
		return err
	}
	if ts, err := time.Parse(time.RFC3339Nano, e.Timestamp); err == nil {
		s.UpdatedAt = ts
	} else {
		s.UpdatedAt = time.Now()
	}
	s.lastID = e.ID
	return nil
}

// AppendMessage records a conversation message and keeps the in-memory history.
func (s *Session) AppendMessage(m agent.Message) error {
	msg := m
	s.Messages = append(s.Messages, msg)
	if s.file == nil {
		return nil // ephemeral modes (-p/--no-session) may have no file
	}
	return s.write(Entry{Type: "message", Message: &msg})
}

// SetName records a new display name.
func (s *Session) SetName(name string) error {
	s.Name = name
	if s.file == nil {
		return nil
	}
	return s.write(Entry{Type: "name", Name: name})
}

// SetModel records a model switch.
func (s *Session) SetModel(model string) error {
	s.Model = model
	if s.file == nil {
		return nil
	}
	return s.write(Entry{Type: "model_change", Model: model})
}

// Close flushes and closes the session file.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

// Load reads a session file and reconstructs its active (linear) branch.
func Load(path string) (*Session, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	s := &Session{Path: path}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var e Entry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a truncated final line
		}
		switch e.Type {
		case "session":
			s.ID = e.SessionID
			s.Cwd = e.Cwd
			s.Model = e.Model
			s.Name = e.Name
		case "message":
			if e.Message != nil {
				s.Messages = append(s.Messages, *e.Message)
			}
		case "name":
			s.Name = e.Name
		case "model_change":
			s.Model = e.Model
		}
		if e.ID != "" {
			s.lastID = e.ID
		}
		if e.Timestamp != "" {
			if ts, perr := time.Parse(time.RFC3339Nano, e.Timestamp); perr == nil && ts.After(s.UpdatedAt) {
				s.UpdatedAt = ts
			}
		}
	}
	if s.ID == "" {
		s.ID = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	if s.Name == "" {
		s.Name = s.ID[:min(8, len(s.ID))]
	}
	return s, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// Latest returns the most recently modified session for a working directory.
func Latest(root, cwd string) (string, error) {
	infos, err := List(root, cwd)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", ErrNoSessions
	}
	return infos[0].Path, nil
}

// List returns sessions for a working directory, newest first.
func List(root, cwd string) ([]Info, error) {
	dir := ProjectDir(root, cwd)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var infos []Info
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		info, err := stat(path)
		if err != nil {
			continue
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
		}
		// deterministic tie-break when timestamps collide
		return infos[i].Path > infos[j].Path
	})
	return infos, nil
}

func stat(path string) (Info, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return Info{}, err
	}
	info := Info{Path: path, UpdatedAt: fi.ModTime()}
	s, err := Load(path)
	if err != nil {
		return info, nil
	}
	info.ID = s.ID
	info.Name = s.Name
	info.Cwd = s.Cwd
	info.Model = s.Model
	info.Messages = len(s.Messages)
	if !s.UpdatedAt.IsZero() {
		info.UpdatedAt = s.UpdatedAt
	}
	return info, nil
}
