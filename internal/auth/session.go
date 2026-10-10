package auth

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
)

// Identifier is the desktop's bundle identifier. Its config directory holds the
// session file the CLI shares with the app (SPEC §3.5).
const Identifier = "dev.atlas.ide"

const (
	sessionFile = "atlas-session.json"
	sessionTemp = "atlas-session.tmp" // path.with_extension("tmp")
)

// Role is an organisation role. Unknown server values decode to nil.
type Role string

// The four roles, highest privilege first. "Owner" is not a role.
const (
	RoleAdmin        Role = "admin"
	RoleProductOwner Role = "product_owner"
	RoleDeveloper    Role = "developer"
	RoleMember       Role = "member"
)

func parseRole(s string) *Role {
	switch r := Role(s); r {
	case RoleAdmin, RoleProductOwner, RoleDeveloper, RoleMember:
		return &r
	}
	return nil
}

// Org is one organisation the user belongs to.
type Org struct {
	ID   string
	Name string
	Role *Role
}

// Identity is the signed-in user as last fetched. Orgs nil means unknown; an
// empty slice means known to be none. The two must never be collapsed.
type Identity struct {
	ID          string
	Name        string
	Email       string
	AvatarURL   *string
	AvatarPath  *string
	Orgs        *[]Org
	ActiveOrgID *string

	extra map[string]json.RawMessage
}

// Pin is the explicit organisation choice. OrgID nil is an explicit "none".
type Pin struct {
	OrgID *string

	extra map[string]json.RawMessage
}

// Session is atlas-session.json. Fields a newer desktop writes are kept
// across a rewrite.
type Session struct {
	SessionToken string
	SavedAt      string
	Identity     *Identity
	PinnedOrg    *Pin

	extra map[string]json.RawMessage
}

func freshSession(token, savedAt string) *Session {
	return &Session{SessionToken: token, SavedAt: savedAt}
}

// ActiveOrgID resolves the active organisation: identity.activeOrgId when it
// is in orgs, else the first org, else none (evidence-auth.md §6.1).
func (s *Session) ActiveOrgID() string {
	if s.Identity == nil || s.Identity.Orgs == nil {
		return ""
	}
	orgs := *s.Identity.Orgs
	if a := s.Identity.ActiveOrgID; a != nil {
		for _, o := range orgs {
			if o.ID == *a {
				return o.ID
			}
		}
	}
	if len(orgs) > 0 {
		return orgs[0].ID
	}
	return ""
}

// ── JSON ─────────────────────────────────────────────────────────────────────

// field is one key of an object written in a fixed order.
type field struct {
	key string
	val any
}

// object marshals its fields in order, then the extras sorted by key.
type object struct {
	fields []field
	extra  map[string]json.RawMessage
}

func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	out := []byte{'{'}
	write := func(k string, v any) error {
		if len(out) > 1 {
			out = append(out, ',')
		}
		b.Reset()
		if err := enc.Encode(k); err != nil {
			return err
		}
		out = append(out, bytes.TrimRight(b.Bytes(), "\n")...)
		out = append(out, ':')
		b.Reset()
		if err := enc.Encode(v); err != nil {
			return err
		}
		out = append(out, bytes.TrimRight(b.Bytes(), "\n")...)
		return nil
	}
	for _, f := range o.fields {
		if err := write(f.key, f.val); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(o.extra))
	for k := range o.extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if err := write(k, o.extra[k]); err != nil {
			return nil, err
		}
	}
	return append(out, '}'), nil
}

// MarshalJSON writes the desktop's field order.
func (o Org) MarshalJSON() ([]byte, error) {
	return object{fields: []field{{"id", o.ID}, {"name", o.Name}, {"role", o.Role}}}.MarshalJSON()
}

// UnmarshalJSON reads the desktop's shape, keeping unknown fields.
func (o *Org) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID   string  `json:"id"`
		Name string  `json:"name"`
		Role *string `json:"role"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	o.ID, o.Name = raw.ID, raw.Name
	o.Role = nil
	if raw.Role != nil {
		o.Role = parseRole(*raw.Role)
	}
	return nil
}

var identityKeys = []string{"id", "name", "email", "avatarUrl", "avatarPath", "orgs", "activeOrgId"}

// MarshalJSON writes the desktop's field order.
func (id Identity) MarshalJSON() ([]byte, error) {
	var orgs any // nil → null; a non-nil empty slice → []
	if id.Orgs != nil {
		list := *id.Orgs
		if list == nil {
			list = []Org{}
		}
		orgs = list
	}
	return object{fields: []field{
		{"id", id.ID}, {"name", id.Name}, {"email", id.Email},
		{"avatarUrl", id.AvatarURL}, {"avatarPath", id.AvatarPath},
		{"orgs", orgs}, {"activeOrgId", id.ActiveOrgID},
	}, extra: id.extra}.MarshalJSON()
}

// UnmarshalJSON reads the desktop's shape, keeping unknown fields.
func (id *Identity) UnmarshalJSON(b []byte) error {
	var raw struct {
		ID          string  `json:"id"`
		Name        string  `json:"name"`
		Email       string  `json:"email"`
		AvatarURL   *string `json:"avatarUrl"`
		AvatarPath  *string `json:"avatarPath"`
		Orgs        *[]Org  `json:"orgs"`
		ActiveOrgID *string `json:"activeOrgId"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	extra, err := extras(b, identityKeys)
	if err != nil {
		return err
	}
	*id = Identity{ID: raw.ID, Name: raw.Name, Email: raw.Email, AvatarURL: raw.AvatarURL,
		AvatarPath: raw.AvatarPath, Orgs: raw.Orgs, ActiveOrgID: raw.ActiveOrgID, extra: extra}
	return nil
}

// MarshalJSON writes the desktop's field order.
func (p Pin) MarshalJSON() ([]byte, error) {
	return object{fields: []field{{"orgId", p.OrgID}}, extra: p.extra}.MarshalJSON()
}

// UnmarshalJSON reads the desktop's shape, keeping unknown fields.
func (p *Pin) UnmarshalJSON(b []byte) error {
	var raw struct {
		OrgID *string `json:"orgId"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	extra, err := extras(b, []string{"orgId"})
	if err != nil {
		return err
	}
	*p = Pin{OrgID: raw.OrgID, extra: extra}
	return nil
}

var sessionKeys = []string{"sessionToken", "savedAt", "identity", "pinnedOrg"}

// MarshalJSON writes the desktop's field order.
func (s Session) MarshalJSON() ([]byte, error) {
	return object{fields: []field{
		{"sessionToken", s.SessionToken}, {"savedAt", s.SavedAt},
		{"identity", s.Identity}, {"pinnedOrg", s.PinnedOrg},
	}, extra: s.extra}.MarshalJSON()
}

// UnmarshalJSON reads the desktop's shape, keeping unknown fields.
func (s *Session) UnmarshalJSON(b []byte) error {
	var raw struct {
		SessionToken *string   `json:"sessionToken"`
		SavedAt      *string   `json:"savedAt"`
		Identity     *Identity `json:"identity"`
		PinnedOrg    *Pin      `json:"pinnedOrg"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw.SessionToken == nil || raw.SavedAt == nil {
		return errors.New("session file: sessionToken and savedAt are required")
	}
	extra, err := extras(b, sessionKeys)
	if err != nil {
		return err
	}
	*s = Session{SessionToken: *raw.SessionToken, SavedAt: *raw.SavedAt,
		Identity: raw.Identity, PinnedOrg: raw.PinnedOrg, extra: extra}
	return nil
}

// extras returns the object's members not in known.
func extras(b []byte, known []string) (map[string]json.RawMessage, error) {
	var all map[string]json.RawMessage
	if err := json.Unmarshal(b, &all); err != nil {
		return nil, err
	}
	for _, k := range known {
		delete(all, k)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// encode writes the file body as serde_json::to_vec_pretty does: two-space
// indent, no HTML escaping, no trailing newline.
func (s *Session) encode() ([]byte, error) {
	compact, err := s.MarshalJSON()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if err := json.Indent(&out, compact, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// ── Store ────────────────────────────────────────────────────────────────────

// Store reads and writes atlas-session.json in one directory.
type Store struct{ Dir string }

// DefaultDir is the app-config directory for the desktop's identifier: macOS
// ~/Library/Application Support/dev.atlas.ide, Linux $XDG_CONFIG_HOME (only
// when absolute) or ~/.config, then dev.atlas.ide.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", Identifier), nil
	case "windows":
		if d := os.Getenv("APPDATA"); d != "" {
			return filepath.Join(d, Identifier), nil
		}
		return "", errors.New("APPDATA is not set")
	default:
		if x := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(x) {
			return filepath.Join(x, Identifier), nil
		}
		return filepath.Join(home, ".config", Identifier), nil
	}
}

// DefaultStore is the store the desktop app reads.
func DefaultStore() (Store, error) {
	d, err := DefaultDir()
	return Store{Dir: d}, err
}

// Path is the session file's path.
func (st Store) Path() string { return filepath.Join(st.Dir, sessionFile) }

// Load reads the session. Any read or parse error means signed out.
func (st Store) Load() (*Session, bool) {
	b, err := os.ReadFile(st.Path())
	if err != nil {
		return nil, false
	}
	var s Session
	if err := json.Unmarshal(b, &s); err != nil || s.SessionToken == "" {
		return nil, false
	}
	return &s, true
}

// Save writes the session atomically: atlas-session.tmp opened 0600, renamed
// over the file, then chmod 0600 again.
func (st Store) Save(s *Session) error {
	body, err := s.encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(st.Dir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", st.Dir, err)
	}
	tmp := filepath.Join(st.Dir, sessionTemp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600) //nolint:gosec // G304: our own config directory
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil { // an existing temp file keeps its old mode
		_ = f.Close()
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, st.Path()); err != nil {
		return err
	}
	return os.Chmod(st.Path(), 0o600)
}

// Clear deletes the session file. A missing file is success.
func (st Store) Clear() error {
	if err := os.Remove(st.Path()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
