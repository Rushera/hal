package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func args(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReadNumbersLinesAndPagination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("one\ntwo\nthree\nfour\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := New("read", dir)

	out, err := tool.Run(context.Background(), args(t, readArgs{Path: "a.txt"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "1| one") || !strings.Contains(out, "4| four") {
		t.Fatalf("expected numbered lines, got:\n%s", out)
	}

	out, err = tool.Run(context.Background(), args(t, readArgs{Path: "a.txt", Offset: 2, Limit: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "1| one") || !strings.Contains(out, "2| two") || !strings.Contains(out, "3| three") || strings.Contains(out, "4| four") {
		t.Fatalf("offset/limit not honoured:\n%s", out)
	}
}

func TestReadRejectsDirectoryAndBinary(t *testing.T) {
	dir := t.TempDir()
	tool := New("read", dir)
	if _, err := tool.Run(context.Background(), args(t, readArgs{Path: "."})); err == nil {
		t.Fatal("expected error reading a directory")
	}
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte{0x00, 0x01, 0x02}, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(context.Background(), args(t, readArgs{Path: "bin"})); err == nil {
		t.Fatal("expected error reading a binary file")
	}
	if _, err := tool.Run(context.Background(), args(t, readArgs{Path: "nope.txt"})); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestWriteCreatesParents(t *testing.T) {
	dir := t.TempDir()
	tool := New("write", dir)
	out, err := tool.Run(context.Background(), args(t, writeArgs{Path: "deep/nested/f.txt", Content: "hello"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wrote 5 bytes") {
		t.Fatalf("unexpected result: %s", out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "deep/nested/f.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "hello" {
		t.Fatalf("content mismatch: %q", data)
	}
}

func TestEditRequiresUniqueMatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.go")
	if err := os.WriteFile(path, []byte("x := 1\nx := 1\ny := 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := New("edit", dir)

	if _, err := tool.Run(context.Background(), args(t, editArgs{Path: "f.go", OldText: "x := 1", NewText: "x := 9"})); err == nil {
		t.Fatal("expected ambiguity error for a duplicated match")
	}

	out, err := tool.Run(context.Background(), args(t, editArgs{Path: "f.go", OldText: "y := 2", NewText: "y := 3"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "replaced 1 occurrence") {
		t.Fatalf("unexpected result: %s", out)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "y := 3") || strings.Contains(string(data), "y := 2") {
		t.Fatalf("edit not applied: %s", data)
	}

	if _, err := tool.Run(context.Background(), args(t, editArgs{Path: "f.go", OldText: "x := 1", NewText: "x := 5", ReplaceAll: true})); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if strings.Count(string(data), "x := 5") != 2 {
		t.Fatalf("replaceAll did not replace both: %s", data)
	}

	if _, err := tool.Run(context.Background(), args(t, editArgs{Path: "f.go", OldText: "absent", NewText: "z"})); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestLsMarksDirectories(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := New("ls", dir).Run(context.Background(), args(t, lsArgs{}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sub/") || !strings.Contains(out, "b.txt") {
		t.Fatalf("unexpected listing:\n%s", out)
	}
}

func TestGrepFindsAndFilters(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("package main\nfunc Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("target here\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "node_modules", "c.go"), []byte("func Target() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := New("grep", dir)

	out, err := tool.Run(context.Background(), args(t, grepArgs{Pattern: "Target", Glob: "*.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go:2:") {
		t.Fatalf("expected match in a.go:\n%s", out)
	}
	if strings.Contains(out, "b.txt") {
		t.Fatalf("glob filter not applied:\n%s", out)
	}
	if strings.Contains(out, "node_modules") {
		t.Fatalf("should skip node_modules:\n%s", out)
	}

	out, err = tool.Run(context.Background(), args(t, grepArgs{Pattern: "TARGET", IgnoreCase: true}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "b.txt:1:") {
		t.Fatalf("ignoreCase not applied:\n%s", out)
	}

	out, err = tool.Run(context.Background(), args(t, grepArgs{Pattern: "nothinghere"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "no matches") {
		t.Fatalf("expected no-match message:\n%s", out)
	}
}

func TestFindGlobs(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src/deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"main.go":                "x",
		"src/util.go":            "x",
		"src/deep/thing_test.go": "x",
		"notes.txt":              "x",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git/objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git/objects/ignored.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := New("find", dir)

	out, err := tool.Run(context.Background(), args(t, findArgs{Pattern: "*.go"}))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"main.go", "src/util.go", "src/deep/thing_test.go"} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected %s in results:\n%s", want, out)
		}
	}
	if strings.Contains(out, ".git") {
		t.Fatalf("should skip .git:\n%s", out)
	}

	out, err = tool.Run(context.Background(), args(t, findArgs{Pattern: "src/**/*_test.go"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "src/deep/thing_test.go") {
		t.Fatalf("** glob failed:\n%s", out)
	}
	if strings.Contains(out, "util.go") {
		t.Fatalf("** glob matched too much:\n%s", out)
	}
}

func TestBashCapturesOutputExitCodeAndWorkdir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	tool := New("bash", dir)

	out, err := tool.Run(context.Background(), args(t, bashArgs{Command: "echo hello && pwd"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hello") || !strings.Contains(out, "exit code 0") {
		t.Fatalf("unexpected output:\n%s", out)
	}

	out, err = tool.Run(context.Background(), args(t, bashArgs{Command: "pwd", Workdir: "sub"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, filepath.Join(dir, "sub")) {
		t.Fatalf("workdir not honoured:\n%s", out)
	}

	out, err = tool.Run(context.Background(), args(t, bashArgs{Command: "echo boom >&2; exit 3"}))
	if err == nil {
		t.Fatal("expected an error for a non-zero exit code")
	}
	if !strings.Contains(out, "boom") || !strings.Contains(out, "exit code 3") {
		t.Fatalf("output should still be reported:\n%s", out)
	}
}

func TestBashTimeoutKillsCommand(t *testing.T) {
	tool := New("bash", t.TempDir())
	start := time.Now()
	out, err := tool.Run(context.Background(), args(t, bashArgs{Command: "sleep 30", TimeoutSeconds: 1}))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected a timeout error")
	}
	if elapsed > 10*time.Second {
		t.Fatalf("timeout did not kill the command, waited %s", elapsed)
	}
	if !strings.Contains(out, "timed out") {
		t.Fatalf("expected a timeout notice:\n%s", out)
	}
}

func TestSelectRejectsUnknownTools(t *testing.T) {
	if _, err := Select(t.TempDir(), "read,nope"); err == nil {
		t.Fatal("expected an error for an unknown tool")
	}
	ts, err := Select(t.TempDir(), "read,grep")
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Name() != "read" || ts[1].Name() != "grep" {
		t.Fatalf("unexpected selection: %v", Names(ts))
	}
	if got := Names(DefaultSet(t.TempDir())); len(got) != 7 {
		t.Fatalf("expected 7 built-in tools, got %v", got)
	}
}

func TestToolSchemasAreValidJSON(t *testing.T) {
	for _, tool := range DefaultSet(t.TempDir()) {
		var schema map[string]any
		if err := json.Unmarshal(tool.Parameters(), &schema); err != nil {
			t.Fatalf("%s: invalid schema: %v", tool.Name(), err)
		}
		if schema["type"] != "object" {
			t.Fatalf("%s: schema type should be object: %s", tool.Name(), tool.Parameters())
		}
		if tool.Description() == "" {
			t.Fatalf("%s: missing description", tool.Name())
		}
	}
}
