package tui

import "os"

// homeDir is split out so the rest of the package stays easy to test.
func homeDir() (string, error) { return os.UserHomeDir() }
