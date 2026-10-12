package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// maxReadBytes caps how much of a file is handed to the model.
const maxReadBytes = 400 * 1024

// resolve turns a tool-supplied path into an absolute path rooted at the
// agent's working directory. "~" expands to the user's home directory.
func resolve(cwd, p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return cwd, nil
	}
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(p, "~"), "/"))
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(cwd, p)
	}
	return filepath.Clean(p), nil
}

// ---- read ------------------------------------------------------------------

type readTool struct{ cwd string }
type readArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (t *readTool) def() toolDef {
	return toolDef{
		name: "read",
		desc: "Read a text file. Output is prefixed with 1-based line numbers (\"   12| text\"). Use offset/limit for long files.",
		params: objSchema(map[string]any{
			"path":   map[string]any{"type": "string", "description": "File path, relative to the working directory or absolute."},
			"offset": map[string]any{"type": "integer", "description": "First line to read (1-based). Default 1."},
			"limit":  map[string]any{"type": "integer", "description": "Maximum number of lines to read. Default 2000."},
		}, "path"),
	}
}
func (t *readTool) Name() string                { d := t.def(); return d.name }
func (t *readTool) Description() string         { d := t.def(); return d.desc }
func (t *readTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

func (t *readTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var a readArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	path, err := resolve(t.cwd, a.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("cannot read %s: %w", display(t.cwd, path), err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory; use the ls tool", display(t.cwd, path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	truncatedBytes := false
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		truncatedBytes = true
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return "", fmt.Errorf("%s looks like a binary file (%s, %d bytes); not reading it as text",
			display(t.cwd, path), info.Mode().String(), info.Size())
	}

	offset := a.Offset
	if offset < 1 {
		offset = 1
	}
	limit := a.Limit
	if limit <= 0 {
		limit = 2000
	}

	var b strings.Builder
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	written := 0
	for sc.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		if written >= limit {
			// keep counting so the header can report the total
			continue
		}
		fmt.Fprintf(&b, "%5d| %s\n", lineNo, sc.Text())
		written++
	}
	if written == 0 {
		return fmt.Sprintf("(no lines in %s at offset %d)", display(t.cwd, path), offset), nil
	}
	header := fmt.Sprintf("%s (lines %d-%d of %d)\n", display(t.cwd, path), offset, offset+written-1, lineNo)
	if truncatedBytes {
		header += fmt.Sprintf("(file truncated to the first %d bytes)\n", maxReadBytes)
	}
	return header + b.String(), nil
}

// ---- write -----------------------------------------------------------------

type writeTool struct{ cwd string }
type writeArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *writeTool) def() toolDef {
	return toolDef{
		name: "write",
		desc: "Create a file or overwrite it completely, creating parent directories as needed. Use edit for targeted changes.",
		params: objSchema(map[string]any{
			"path":    map[string]any{"type": "string", "description": "File path to write."},
			"content": map[string]any{"type": "string", "description": "Full file content."},
		}, "path", "content"),
	}
}
func (t *writeTool) Name() string                { d := t.def(); return d.name }
func (t *writeTool) Description() string         { d := t.def(); return d.desc }
func (t *writeTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

func (t *writeTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var a writeArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	path, err := resolve(t.cwd, a.Path)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return "", fmt.Errorf("%s is a directory", display(t.cwd, path))
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	if err := os.WriteFile(path, []byte(a.Content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), display(t.cwd, path)), nil
}

// ---- edit ------------------------------------------------------------------

type editTool struct{ cwd string }
type editArgs struct {
	Path       string `json:"path"`
	OldText    string `json:"oldText"`
	NewText    string `json:"newText"`
	ReplaceAll bool   `json:"replaceAll"`
}

func (t *editTool) def() toolDef {
	return toolDef{
		name: "edit",
		desc: "Apply an exact text replacement to an existing file. oldText must match the file exactly and, unless replaceAll is true, must be unique. Read the file first.",
		params: objSchema(map[string]any{
			"path":       map[string]any{"type": "string", "description": "File to edit."},
			"oldText":    map[string]any{"type": "string", "description": "Exact text to replace, including indentation."},
			"newText":    map[string]any{"type": "string", "description": "Replacement text. Empty deletes the match."},
			"replaceAll": map[string]any{"type": "boolean", "description": "Replace every occurrence instead of requiring a unique match."},
		}, "path", "oldText", "newText"),
	}
}
func (t *editTool) Name() string                { d := t.def(); return d.name }
func (t *editTool) Description() string         { d := t.def(); return d.desc }
func (t *editTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

func (t *editTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var a editArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Path) == "" {
		return "", fmt.Errorf("path is required")
	}
	if a.OldText == "" {
		return "", fmt.Errorf("oldText is required and must not be empty")
	}
	path, err := resolve(t.cwd, a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("cannot edit %s: %w", display(t.cwd, path), err)
	}
	content := string(data)
	count := strings.Count(content, a.OldText)
	switch {
	case count == 0:
		return "", fmt.Errorf("oldText not found in %s; the file may have changed. Read it again and copy the exact text", display(t.cwd, path))
	case count > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("oldText appears %d times in %s; include more surrounding context to make it unique or set replaceAll", count, display(t.cwd, path))
	}
	updated := strings.Replace(content, a.OldText, a.NewText, 1)
	if a.ReplaceAll {
		updated = strings.ReplaceAll(content, a.OldText, a.NewText)
	}
	info, err := os.Stat(path)
	mode := os.FileMode(0o644)
	if err == nil {
		mode = info.Mode().Perm()
	}
	if err := os.WriteFile(path, []byte(updated), mode); err != nil {
		return "", err
	}
	verb := "occurrence"
	if count != 1 {
		verb = "occurrences"
	}
	return fmt.Sprintf("edited %s: replaced %d %s", display(t.cwd, path), count, verb), nil
}

// ---- ls --------------------------------------------------------------------

type lsTool struct{ cwd string }
type lsArgs struct {
	Path string `json:"path"`
}

func (t *lsTool) def() toolDef {
	return toolDef{
		name: "ls",
		desc: "List directory contents. Directories are marked with a trailing slash.",
		params: objSchema(map[string]any{
			"path": map[string]any{"type": "string", "description": "Directory to list. Defaults to the working directory."},
		}),
	}
}
func (t *lsTool) Name() string                { d := t.def(); return d.name }
func (t *lsTool) Description() string         { d := t.def(); return d.desc }
func (t *lsTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

func (t *lsTool) Run(_ context.Context, raw json.RawMessage) (string, error) {
	var a lsArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	path, err := resolve(t.cwd, a.Path)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("cannot list %s: %w", display(t.cwd, path), err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		names = append(names, name)
	}
	sort.Strings(names)
	const maxEntries = 1000
	truncated := false
	if len(names) > maxEntries {
		names = names[:maxEntries]
		truncated = true
	}
	out := fmt.Sprintf("%s (%d entries)\n", display(t.cwd, path), len(entries)) + strings.Join(names, "\n")
	if truncated {
		out += fmt.Sprintf("\n... (showing first %d)", maxEntries)
	}
	return out, nil
}

// display renders a path relative to the working directory when possible.
func display(cwd, path string) string {
	if rel, err := filepath.Rel(cwd, path); err == nil && !strings.HasPrefix(rel, "..") {
		return rel
	}
	return path
}
