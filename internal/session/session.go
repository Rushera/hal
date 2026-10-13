// Package session persists conversations as JSONL, one entry per line with
// parent links. hal keeps the tree but only uses the linear active branch.
package session

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
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

// SearchRoots returns the directories to search when resuming a session.
func SearchRoots(explicit string) []string {
	return []string{RootWith(explicit)}
}

// LatestIn returns the most recent session across the given roots.
func LatestIn(roots []string, cwd string, all bool) (string, error) {
	var newest string
	var newestTime time.Time
	for _, root := range roots {
		infos, err := List(root, cwd, all)
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

// FindByID locates the file of the session with the given id anywhere under
// root. Both the full id and the 8-character prefix used in file names are
// accepted, which is what lets the web console reopen the session named in a
// page URL.
func FindByID(root, id string) (string, error) {
	root, id = strings.TrimSpace(root), strings.TrimSpace(id)
	if root == "" || id == "" {
		return "", ErrNoSessions
	}
	short := id
	if len(short) > 8 {
		short = short[:8]
	}
	suffix := "-" + short + ".jsonl"
	var found string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), suffix) {
			return nil
		}
		s, err := Load(path)
		if err != nil {
			return nil
		}
		if s.ID == id || strings.HasPrefix(s.ID, id) {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	if found == "" {
		return "", ErrNoSessions
	}
	return found, nil
}

// Latest returns the most recently modified session for a working directory.
func Latest(root, cwd string) (string, error) {
	infos, err := List(root, cwd, false)
	if err != nil {
		return "", err
	}
	if len(infos) == 0 {
		return "", ErrNoSessions
	}
	return infos[0].Path, nil
}

// List returns sessions, newest first. It covers the working directory's own
// bucket, or every bucket under root when all is set, which is what the session
// switchers use to show the conversations stored for this machine.
func List(root, cwd string, all bool) ([]Info, error) {
	if all {
		return listAll(root, cwd)
	}
	return listDir(ProjectDir(root, cwd))
}

// listAll walks every project bucket under root and keeps the sessions whose
// recorded cwd is wanted (when wanted is not empty).
func listAll(root, wanted string) ([]Info, error) {
	if strings.TrimSpace(wanted) != "" {
		if abs, err := filepath.Abs(wanted); err == nil {
			wanted = abs
		}
	}
	var infos []Info
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, serr := stat(path)
		if serr != nil {
			return nil
		}
		if wanted != "" && info.Cwd != wanted {
			return nil
		}
		infos = append(infos, info)
		return nil
	})
	sortInfos(infos)
	return infos, nil
}

func listDir(dir string) ([]Info, error) {
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
	sortInfos(infos)
	return infos, nil
}

// sortInfos orders sessions newest first, with a deterministic tie-break when
// two timestamps collide.
func sortInfos(infos []Info) {
	sort.Slice(infos, func(i, j int) bool {
		if !infos[i].UpdatedAt.Equal(infos[j].UpdatedAt) {
			return infos[i].UpdatedAt.After(infos[j].UpdatedAt)
		}
		return infos[i].Path > infos[j].Path
	})
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
