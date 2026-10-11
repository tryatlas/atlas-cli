// Package ingest talks to the ingest host: cloud Projects ("workspaces" in the
// server's vocabulary) now, the capture drain later (SPEC §4.4, §6.7).
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

const timeout = 30 * time.Second

// Tokens supplies access JWTs. Invalidate is called once after a 401.
type Tokens interface {
	Token(ctx context.Context) (string, error)
	Invalidate()
}

// Client calls the ingest host with a bearer JWT.
type Client struct {
	Base   string
	Tokens Tokens
	HTTP   *http.Client
}

// New returns a client with a 30 s timeout.
func New(base string, tokens Tokens) *Client {
	return &Client{Base: base, Tokens: tokens, HTTP: &http.Client{Timeout: timeout}}
}

// Error is a non-2xx answer. Body codes come from {"error":{"code","message"}}.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("Atlas said %d %s: %s", e.Status, e.Code, e.Message)
	}
	if e.Code != "" {
		return fmt.Sprintf("Atlas said %d %s", e.Status, e.Code)
	}
	return fmt.Sprintf("Atlas said %d", e.Status)
}

// StatusOf returns the HTTP status of an *Error, else 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// do sends one JSON call, refreshing the JWT once on a 401.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	for attempt := 0; ; attempt++ {
		jwt, err := c.Tokens.Token(ctx)
		if err != nil {
			return err
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.Base+path, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+jwt)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // never echo the URL
			}
			return fmt.Errorf("could not reach Atlas ingest: %w", err)
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.Tokens.Invalidate()
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			e := &Error{Status: resp.StatusCode}
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal(b, &env) == nil {
				e.Code, e.Message = env.Error.Code, env.Error.Message
			}
			return e
		}
		if out != nil && len(bytes.TrimSpace(b)) > 0 {
			if err := json.Unmarshal(b, out); err != nil {
				return fmt.Errorf("unreadable answer from Atlas ingest: %w", err)
			}
		}
		return nil
	}
}

// Workspace is a cloud Project.
type Workspace struct {
	ID            string  `json:"id"`
	Slug          string  `json:"slug"`
	Name          *string `json:"name,omitempty"`
	RootCommitSHA *string `json:"rootCommitSha,omitempty"`
	GitURL        *string `json:"gitUrl,omitempty"`
	Visibility    *string `json:"visibility,omitempty"`
}

// Label is the name, else the slug.
func (w Workspace) Label() string {
	if w.Name != nil && *w.Name != "" {
		return *w.Name
	}
	return w.Slug
}

// Workspaces lists an organisation's cloud Projects.
func (c *Client) Workspaces(ctx context.Context, orgID string) ([]Workspace, error) {
	var out struct {
		Workspaces []Workspace `json:"workspaces"`
	}
	err := c.do(ctx, http.MethodGet, "/workspaces?orgId="+url.QueryEscape(orgID), nil, &out)
	return out.Workspaces, err
}

// SlugAvailable asks whether a Project slug is free in an organisation. A 409
// also means taken.
func (c *Client) SlugAvailable(ctx context.Context, orgID, slug string) (bool, error) {
	var out struct {
		Available bool `json:"available"`
	}
	err := c.do(ctx, http.MethodGet, "/workspaces/slug-available?orgId="+url.QueryEscape(orgID)+"&slug="+url.QueryEscape(slug), nil, &out)
	if StatusOf(err) == http.StatusConflict {
		return false, nil
	}
	return out.Available, err
}

// CreateRequest is POST /workspaces. Optional fields are omitted, never null.
type CreateRequest struct {
	OrgID         string  `json:"orgId"`
	Slug          string  `json:"slug"`
	Visibility    string  `json:"visibility"` // "org" | "restricted"
	Name          *string `json:"name,omitempty"`
	RootCommitSHA *string `json:"rootCommitSha,omitempty"`
	GitURL        *string `json:"gitUrl,omitempty"`
}

// ErrSlugTaken is a 409 on create.
var ErrSlugTaken = errors.New("that Project handle is taken in this organisation")

// Create makes a cloud Project and returns its id.
func (c *Client) Create(ctx context.Context, req CreateRequest) (string, error) {
	var out struct {
		WorkspaceID string `json:"workspaceId"`
		ID          string `json:"id"`
	}
	err := c.do(ctx, http.MethodPost, "/workspaces", req, &out)
	if StatusOf(err) == http.StatusConflict {
		return "", ErrSlugTaken
	}
	if err != nil {
		return "", err
	}
	if out.WorkspaceID != "" {
		return out.WorkspaceID, nil
	}
	if out.ID != "" {
		return out.ID, nil
	}
	return "", errors.New("the ingest server created the Project but returned no id")
}

// ConnectRequest is POST /workspaces/connect.
type ConnectRequest struct {
	OrgID         string  `json:"orgId"`
	Create        bool    `json:"create"`
	WorkspaceID   *string `json:"workspaceId,omitempty"`
	Slug          *string `json:"slug,omitempty"`
	RootCommitSHA *string `json:"rootCommitSha,omitempty"`
	GitURL        *string `json:"gitUrl,omitempty"`
}

// ConnectResult is the server's match: Status "connected" with WorkspaceID
// and Reason, "ambiguous" with Candidates, or anything else for no match.
type ConnectResult struct {
	Status      string      `json:"status"`
	WorkspaceID string      `json:"workspaceId"`
	Slug        *string     `json:"slug"`
	Reason      string      `json:"reason"`
	Candidates  []Workspace `json:"candidates"`
}

// Connect lets the server match this repository to a Project.
func (c *Client) Connect(ctx context.Context, req ConnectRequest) (*ConnectResult, error) {
	var out ConnectResult
	if err := c.do(ctx, http.MethodPost, "/workspaces/connect", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
