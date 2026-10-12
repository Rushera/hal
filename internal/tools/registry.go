// Package tools implements hal's built-in tools: read, bash, edit, write, grep,
// find and ls. A Windows-only powershell tool is not included.
package tools

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Rushera/hal/internal/agent"
)

// BuiltinNames lists the tools hal knows about.
var BuiltinNames = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}

// DefaultNames is the default enabled set: read/bash/edit/write plus the
// read-only search helpers.
var DefaultNames = []string{"read", "bash", "edit", "write", "grep", "find", "ls"}

// DefaultSet returns one instance of every built-in tool bound to cwd.
func DefaultSet(cwd string) []agent.Tool {
	out := make([]agent.Tool, 0, len(DefaultNames))
	for _, name := range DefaultNames {
		out = append(out, New(name, cwd))
	}
	return out
}

// New builds a single built-in tool by name. Unknown names return nil.
func New(name, cwd string) agent.Tool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "read":
		return &readTool{cwd}
	case "write":
		return &writeTool{cwd}
	case "edit":
		return &editTool{cwd}
	case "bash":
		return &bashTool{cwd}
	case "ls":
		return &lsTool{cwd}
	case "grep":
		return &grepTool{cwd}
	case "find":
		return &findTool{cwd}
	}
	return nil
}

// Select resolves a comma-separated allowlist (as passed to --tools). An empty
// list means "all built-ins".
func Select(cwd, list string) ([]agent.Tool, error) {
	if strings.TrimSpace(list) == "" {
		return DefaultSet(cwd), nil
	}
	var out []agent.Tool
	var unknown []string
	for _, raw := range strings.Split(list, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		t := New(name, cwd)
		if t == nil {
			unknown = append(unknown, name)
			continue
		}
		out = append(out, t)
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("unknown tool(s): %s (available: %s)",
			strings.Join(unknown, ", "), strings.Join(BuiltinNames, ", "))
	}
	return out, nil
}

// toolDef carries the static metadata every built-in tool shares.
type toolDef struct {
	name   string
	desc   string
	params map[string]any
}

func (t *toolDef) Name() string        { return t.name }
func (t *toolDef) Description() string { return t.desc }

// Parameters renders the JSON Schema for the tool arguments.
func (t *toolDef) Parameters() json.RawMessage {
	b, err := json.Marshal(t.params)
	if err != nil {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return b
}

func objSchema(props map[string]any, required ...string) map[string]any {
	m := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		m["required"] = required
	}
	return m
}

// Names returns the tool names of a slice, sorted.
func Names(ts []agent.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	sort.Strings(out)
	return out
}

func decodeArgs(raw json.RawMessage, dst any) error {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		trimmed = "{}"
	}
	if err := json.Unmarshal([]byte(trimmed), dst); err != nil {
		return fmt.Errorf("invalid tool arguments: %w (got %s)", err, trimmed)
	}
	return nil
}
