package auth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/tryatlas/atlas-cli/internal/hosts"
	"github.com/tryatlas/atlas-cli/internal/timefmt"
)

// ErrSignedOut means there is no usable session file.
var ErrSignedOut = errors.New("not signed in; run atlas login")

// Auth ties the session file to the auth host.
type Auth struct {
	Store  Store
	Client *Client

	clk clock
	mu  sync.Mutex // serialises load → modify → save
	ts  *TokenSource
}

// New uses the desktop's session file and the configured auth host.
func New() (*Auth, error) {
	st, err := DefaultStore()
	if err != nil {
		return nil, err
	}
	return &Auth{Store: st, Client: NewClient(hosts.Auth()), clk: realClock}, nil
}

// NewWith uses the given store and client (tests, other profiles).
func NewWith(st Store, c *Client) *Auth {
	return &Auth{Store: st, Client: c, clk: realClock}
}

// Session returns the stored session, or ErrSignedOut.
func (a *Auth) Session() (*Session, error) {
	s, ok := a.Store.Load()
	if !ok {
		return nil, ErrSignedOut
	}
	return s, nil
}

// Login runs the device grant, writes the session file the way the desktop
// does (identity and pin cleared), then fetches the identity. A failure after
// the token is written never touches the credential.
func (a *Auth) Login(ctx context.Context, show func(Prompt)) (*Session, error) {
	token, err := runGrant(ctx, a.Client, a.clk, show)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	err = a.Store.Save(freshSession(token, timefmt.RFC3339(a.clk.now())))
	a.mu.Unlock()
	if err != nil {
		return nil, err
	}
	a.ts = nil
	s, _ := a.Refresh(ctx)
	if s == nil {
		s, _ = a.Store.Load()
	}
	return s, nil
}

// Refresh fetches the profile, the organisation list and the roles from a
// fresh JWT, and rewrites the identity (evidence-auth.md §8 "After sign-in").
// Failures keep what is stored; only a 401 from GET /token clears the session.
func (a *Auth) Refresh(ctx context.Context) (*Session, error) {
	s, err := a.Session()
	if err != nil {
		return nil, err
	}
	token := s.SessionToken

	profile, profErr := a.Client.GetSession(ctx, token)
	listing, listErr := a.Client.ListOrgs(ctx, token)
	jwt, jwtErr := a.Client.MintJWT(ctx, token)
	if IsRejected(jwtErr) {
		a.mu.Lock()
		_ = a.Store.Clear()
		a.mu.Unlock()
		return nil, ErrSignedOut
	}
	claims, claimsOK := Claims{}, false
	if jwtErr == nil {
		claims, claimsOK = ParseClaims(jwt)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	// Re-read after the network calls: a sign-out that happened meanwhile wins,
	// and a newer token written by the desktop is not overwritten.
	cur, ok := a.Store.Load()
	if !ok || cur.SessionToken != token {
		return cur, nil
	}
	id := mergeIdentity(cur, profile, profErr == nil, listing, listErr == nil, claims, claimsOK)
	if id == nil {
		return cur, firstErr(profErr, listErr, jwtErr)
	}
	cur.Identity = id
	if err := a.Store.Save(cur); err != nil {
		return nil, err
	}
	return cur, firstErr(profErr, listErr, jwtErr)
}

func firstErr(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// mergeIdentity builds the new identity from what was fetched and what was
// stored. It returns nil when there is no identity to write (no profile yet).
func mergeIdentity(cur *Session, profile *Profile, profOK bool, listing []OrgListing, listOK bool, claims Claims, claimsOK bool) *Identity {
	old := cur.Identity
	var id Identity
	switch {
	case profOK && profile != nil:
		id = Identity{ID: profile.UserID, Name: profile.Name, Email: profile.Email, AvatarURL: profile.Image}
		if old != nil {
			id.extra = old.extra
			// The avatar file is the desktop's cache; keep it while the URL is unchanged.
			if eqPtr(old.AvatarURL, profile.Image) {
				id.AvatarPath = old.AvatarPath
			}
		}
	case old != nil:
		id = *old
	default:
		return nil
	}

	// Organisations: names from the list, roles from the JWT claim. A failed
	// list keeps the stored one; an unreadable JWT keeps the stored roles.
	id.Orgs = nil
	if old != nil {
		id.Orgs = old.Orgs
	}
	if listOK {
		prevRole := map[string]*Role{}
		if old != nil && old.Orgs != nil {
			for _, o := range *old.Orgs {
				prevRole[o.ID] = o.Role
			}
		}
		orgs := make([]Org, 0, len(listing))
		for _, l := range listing {
			o := Org{ID: l.ID, Name: l.Name}
			if claimsOK {
				if r, ok := claims.Orgs[l.ID]; ok {
					o.Role = parseRole(r)
				}
			} else {
				o.Role = prevRole[l.ID]
			}
			orgs = append(orgs, o)
		}
		id.Orgs = &orgs
	} else if claimsOK && id.Orgs != nil {
		orgs := append([]Org(nil), *id.Orgs...)
		for i := range orgs {
			if r, ok := claims.Orgs[orgs[i].ID]; ok {
				orgs[i].Role = parseRole(r)
			} else {
				orgs[i].Role = nil
			}
		}
		id.Orgs = &orgs
	}

	// Active org: a choice already made (or pinned) is kept; the web's
	// activeOrganizationId only seeds a session that never chose.
	switch {
	case old != nil && old.ActiveOrgID != nil:
		id.ActiveOrgID = old.ActiveOrgID
	case cur.PinnedOrg != nil:
		id.ActiveOrgID = cur.PinnedOrg.OrgID
	case profOK && profile != nil:
		id.ActiveOrgID = profile.ActiveOrganizationID
	default:
		id.ActiveOrgID = nil
	}
	return &id
}

func eqPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// SetActiveOrg pins the organisation locally (pinnedOrg and
// identity.activeOrgId). The server is not told. orgID "" pins "none".
func (a *Auth) SetActiveOrg(orgID string) (*Session, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.Store.Load()
	if !ok {
		return nil, ErrSignedOut
	}
	var id *string
	if orgID != "" {
		id = &orgID
	}
	pin := Pin{OrgID: id}
	if s.PinnedOrg != nil {
		pin.extra = s.PinnedOrg.extra
	}
	s.PinnedOrg = &pin
	if s.Identity != nil {
		s.Identity.ActiveOrgID = id
	}
	return s, a.Store.Save(s)
}

// Logout deletes the local file (and the desktop's avatar cache file) first,
// then revokes the session on the server once. It reports whether the server
// confirmed; false means the server session may outlive the local one.
func (a *Auth) Logout(ctx context.Context) (bool, error) {
	a.mu.Lock()
	s, ok := a.Store.Load()
	err := a.Store.Clear()
	if ok && s.Identity != nil && s.Identity.AvatarPath != nil {
		removeAvatar(a.Store.Dir, *s.Identity.AvatarPath)
	}
	a.mu.Unlock()
	a.ts = nil
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrSignedOut
	}
	return a.Client.SignOut(ctx, s.SessionToken), nil
}

// removeAvatar deletes an avatar-*.ext file only if it sits in dir.
func removeAvatar(dir, path string) {
	if filepath.Dir(path) != filepath.Clean(dir) || !strings.HasPrefix(filepath.Base(path), "avatar-") {
		return
	}
	_ = os.Remove(path)
}

// Tokens returns the JWT source for other hosts. A 401 on GET /token clears
// the session.
func (a *Auth) Tokens() *TokenSource {
	if a.ts == nil {
		a.ts = NewTokenSource(func(ctx context.Context) (string, error) {
			s, err := a.Session()
			if err != nil {
				return "", err
			}
			jwt, err := a.Client.MintJWT(ctx, s.SessionToken)
			if IsRejected(err) {
				a.mu.Lock()
				if cur, ok := a.Store.Load(); ok && cur.SessionToken == s.SessionToken {
					_ = a.Store.Clear()
				}
				a.mu.Unlock()
				return "", ErrSignedOut
			}
			return jwt, err
		})
	}
	return a.ts
}
