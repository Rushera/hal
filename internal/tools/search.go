package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// skipDirs are directories the search tools never descend into: version control
// metadata and dependency trees, where matches are noise.
var skipDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true, ".bzr": true,
	"node_modules": true, "bower_components": true, "vendor": true,
	"__pycache__": true, ".venv": true, "venv": true, "site-packages": true,
	".mypy_cache": true, ".pytest_cache": true, ".ruff_cache": true,
	".gradle": true, ".terraform": true, ".cache": true, ".next": true,
	".tox": true, ".venv-hermes": true,
}

const (
	maxGrepFileSize = 2 * 1024 * 1024
	maxGrepResults  = 2000
)

// ---- grep ------------------------------------------------------------------

type grepTool struct{ cwd string }

type grepArgs struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignoreCase"`
	MaxResults int    `json:"maxResults"`
}

func (t *grepTool) def() toolDef {
	return toolDef{
		name: "grep",
		desc: "Search file contents with a regular expression (falls back to a literal match if the pattern is not valid regex). Returns \"path:line: text\" matches. Skips VCS metadata and dependency directories.",
		params: objSchema(map[string]any{
			"pattern":    map[string]any{"type": "string", "description": "Regular expression to search for."},
			"path":       map[string]any{"type": "string", "description": "File or directory to search. Defaults to the working directory."},
			"glob":       map[string]any{"type": "string", "description": "Only search files matching this glob, e.g. \"*.go\"."},
			"ignoreCase": map[string]any{"type": "boolean", "description": "Case-insensitive match."},
			"maxResults": map[string]any{"type": "integer", "description": "Maximum matches to return (default 200)."},
		}, "pattern"),
	}
}
func (t *grepTool) Name() string                { d := t.def(); return d.name }
func (t *grepTool) Description() string         { d := t.def(); return d.desc }
func (t *grepTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

func (t *grepTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var a grepArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if a.Pattern == "" {
		return "", fmt.Errorf("pattern is required")
	}
	expr := a.Pattern
	if a.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		re = regexp.MustCompile(regexp.QuoteMeta(a.Pattern))
	}
	limit := a.MaxResults
	if limit <= 0 {
		limit = 200
	}
	if limit > maxGrepResults {
		limit = maxGrepResults
	}
	root, err := resolve(t.cwd, a.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", fmt.Errorf("cannot grep %s: %w", display(t.cwd, root), err)
	}

	var matches []string
	truncated := false
	addFile := func(path string) {
		if truncated {
			return
		}
		hits, err := grepFile(path, re, limit-len(matches), t.cwd)
		if err != nil {
			return
		}
		for _, h := range hits {
			matches = append(matches, h)
			if len(matches) >= limit {
				truncated = true
				return
			}
		}
	}

	if info.IsDir() {
		err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			name := d.Name()
			if d.IsDir() {
				if path != root && (skipDirs[name] || strings.HasPrefix(name, ".") && name != ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			if !matchGlob(a.Glob, name, path, root) {
				return nil
			}
			if truncated {
				return filepath.SkipAll
			}
			addFile(path)
			return nil
		})
		if err != nil && ctx.Err() != nil {
			return "", ctx.Err()
		}
	} else {
		addFile(root)
	}

	if len(matches) == 0 {
		return fmt.Sprintf("no matches for %q in %s", a.Pattern, display(t.cwd, root)), nil
	}
	out := strings.Join(matches, "\n")
	if truncated {
		out += fmt.Sprintf("\n... (result limit of %d reached)", limit)
	}
	return fmt.Sprintf("%d match(es) for %q in %s:\n%s", len(matches), a.Pattern, display(t.cwd, root), out), nil
}

func grepFile(path string, re *regexp.Regexp, budget int, cwd string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil || info.Size() > maxGrepFileSize {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return nil, nil // binary
	}
	var out []string
	rel := display(cwd, path)
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if re.MatchString(line) {
			out = append(out, fmt.Sprintf("%s:%d: %s", rel, lineNo, strings.TrimRight(line, " \t\r")))
			if len(out) >= budget {
				break
			}
		}
	}
	return out, nil
}

// ---- find ------------------------------------------------------------------

type findTool struct{ cwd string }

type findArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
}

func (t *findTool) def() toolDef {
	return toolDef{
		name: "find",
		desc: "Find files and directories by glob pattern, e.g. \"*.go\", \"src/**/*_test.go\". \"**\" matches across directories. Skips VCS metadata and dependency directories.",
		params: objSchema(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "Glob pattern. Without a slash it matches file names at any depth."},
			"path":    map[string]any{"type": "string", "description": "Directory to search. Defaults to the working directory."},
		}, "pattern"),
	}
}
func (t *findTool) Name() string                { d := t.def(); return d.name }
func (t *findTool) Description() string         { d := t.def(); return d.desc }
func (t *findTool) Parameters() json.RawMessage { d := t.def(); return d.Parameters() }

func (t *findTool) Run(ctx context.Context, raw json.RawMessage) (string, error) {
	var a findArgs
	if err := decodeArgs(raw, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return "", fmt.Errorf("pattern is required")
	}
	root, err := resolve(t.cwd, a.Path)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(root); err != nil {
		return "", fmt.Errorf("cannot search %s: %w", display(t.cwd, root), err)
	}
	re, err := globToRegexp(a.Pattern)
	if err != nil {
		return "", err
	}
	baseOnly := !strings.Contains(a.Pattern, "/")
	const maxResults = 1000
	var results []string
	truncated := false

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			if path != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		target := rel
		if baseOnly {
			target = d.Name()
		}
		if re.MatchString(target) {
			entry := rel
			if d.IsDir() {
				entry += "/"
			}
			results = append(results, entry)
			if len(results) >= maxResults {
				truncated = true
				return filepath.SkipAll
			}
		}
		return nil
	})
	if err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	sort.Strings(results)
	if len(results) == 0 {
		return fmt.Sprintf("no paths matching %q in %s", a.Pattern, display(t.cwd, root)), nil
	}
	out := fmt.Sprintf("%d path(s) matching %q in %s:\n%s",
		len(results), a.Pattern, display(t.cwd, root), strings.Join(results, "\n"))
	if truncated {
		out += fmt.Sprintf("\n... (result limit of %d reached)", maxResults)
	}
	return out, nil
}

// globToRegexp converts a shell-style glob into a regular expression, where
// "**" crosses directory separators and "*"/"?" do not.
func globToRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		c := pattern[i]
		switch c {
		case '*':
			if i+1 < len(pattern) && pattern[i+1] == '*' {
				i++
				// "**/" matches zero or more directories
				if i+1 < len(pattern) && pattern[i+1] == '/' {
					i++
					b.WriteString("(?:.*/)?")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '(', ')', '|', '^', '$', '{', '}', '[', ']', '\\':
			b.WriteString("\\" + string(c))
		default:
			b.WriteByte(c)
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func matchGlob(glob, name, path, root string) bool {
	if strings.TrimSpace(glob) == "" {
		return true
	}
	if ok, _ := filepath.Match(glob, name); ok {
		return true
	}
	re, err := globToRegexp(glob)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return re.MatchString(filepath.ToSlash(rel))
}
