package clistate

import (
	"encoding/json"
	"fmt"
	"os"
)

// Prefs are the CLI's per-Project choices that are not the desktop's
// (.atlas/ stays the app's).
type Prefs struct {
	// BackgroundSync keeps atlas serve running for the Project: atlas sync on
	// starts it, and an agent hook restarts it when it is gone.
	BackgroundSync bool `json:"backgroundSync"`
}

const prefsFile = "prefs.json"

// LoadPrefs reads the Project's prefs; a missing or unreadable file is the
// zero value.
func LoadPrefs(root string) Prefs {
	var p Prefs
	if b, err := os.ReadFile(Path(root, prefsFile)); err == nil {
		_ = json.Unmarshal(b, &p)
	}
	return p
}

// SavePrefs writes them.
func SavePrefs(root string, p Prefs) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	path := Path(root, prefsFile)
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
