package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/auth"
	"github.com/tryatlas/atlas-cli/internal/capture/store"
	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/embed"
	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/hooks/mcpconfig"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui/settings"
)

// atlas-cli settings: account, Project gates, agents and background sync on
// one screen. Without a terminal it prints the same rows.
func newSettingsCmd() *cobra.Command {
	return withRun(leaf("settings", "Change this Project's capture, shared memory, agents and background sync on one screen", cobra.NoArgs), runSettings)
}

func runSettings(cmd *cobra.Command, _ []string) error {
	ctx := cmd.Context()
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	be := &settingsBackend{cwd: cwd}
	title := cwd
	if p, err := project.Discover(ctx, cwd); err == nil {
		title = p.Root
	}
	if !interactive() {
		secs, err := be.Load(ctx)
		if err != nil {
			return err
		}
		printSettings(cmd.OutOrStdout(), secs)
		return nil
	}
	_, err = tea.NewProgram(settings.New(ctx, be, title), tea.WithContext(ctx)).Run()
	return err
}

// printSettings is the screen as text, for pipes and scripts.
func printSettings(w io.Writer, secs []settings.Section) {
	for i, s := range secs {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, s.Title)
		for _, it := range s.Items {
			v := it.Value
			switch it.Kind {
			case settings.Toggle:
				st := "off"
				if it.On {
					st = "on"
				}
				v = strings.TrimSpace(st + "  " + it.Value)
			case settings.Action:
				continue
			}
			fmt.Fprintf(w, "  %-22s %s\n", it.Label, v)
		}
	}
}

// settingsBackend reads the state the commands read, and changes it through
// the commands themselves, so the screen and the CLI cannot disagree.
type settingsBackend struct{ cwd string }

func (b *settingsBackend) project(ctx context.Context) *project.Project {
	p, err := project.Discover(ctx, b.cwd)
	if err != nil {
		return nil
	}
	return p
}

const needInit = "Run atlas-cli init in this folder first."

// Load builds the sections.
func (b *settingsBackend) Load(ctx context.Context) ([]settings.Section, error) {
	p := b.project(ctx)
	secs := []settings.Section{b.account(), b.projectSection(ctx, p), b.agents(p), b.sync(ctx, p), b.model(p), b.about(p)}
	return secs, nil
}

func (b *settingsBackend) account() settings.Section {
	sec := settings.Section{Title: "Account", Icon: "◉"}
	a, err := newAuth()
	var s *auth.Session
	if err == nil {
		s, _ = a.Session()
	}
	if s == nil {
		sec.Items = []settings.Item{{Label: "Signed in as", Value: "not signed in",
			Detail: "Run atlas-cli login to sync sessions and memory to a cloud Project."}}
		return sec
	}
	who := "signed in"
	if s.Identity != nil {
		who = fmt.Sprintf("%s <%s>", s.Identity.Name, s.Identity.Email)
	}
	org := "none chosen"
	if id := s.ActiveOrgID(); id != "" {
		org = orgLabel(s, id)
	}
	n := 0
	if s.Identity != nil && s.Identity.Orgs != nil {
		n = len(*s.Identity.Orgs)
	}
	sec.Items = []settings.Item{
		{Label: "Signed in as", Value: who, Detail: "atlas-cli logout signs out and deletes the local session file."},
		{Label: "Active organisation", Value: org, Detail: "atlas-cli org use <id> changes it; atlas-cli org list shows them."},
		{Label: "Organisations", Value: fmt.Sprint(n)},
		{Label: "Session file", Value: a.Store.Path(), Detail: "The only secret Atlas keeps on disk (mode 0600)."},
	}
	return sec
}

func (b *settingsBackend) projectSection(ctx context.Context, p *project.Project) settings.Section {
	sec := settings.Section{Title: "Project", Icon: "▣"}
	var dir string
	if p == nil {
		dir, _ = project.Canonical(b.cwd)
		sec.Items = append(sec.Items, settings.Item{Label: "Folder", Value: dir + " (not initialised)", Detail: needInit})
	} else {
		dir = p.Root
		sec.Items = append(sec.Items, settings.Item{Label: "Folder", Value: p.Root})
	}
	var bind *store.Binding
	if p != nil {
		if st, err := store.Open(p.Root, false); err == nil {
			bind, _ = st.Binding(ctx)
			_ = st.Close()
		}
		var sess *auth.Session
		if a, err := newAuth(); err == nil {
			sess, _ = a.Session()
		}
		if bind != nil {
			sec.Items = append(sec.Items, settings.Item{Label: "Bound to", Value: describeBinding(bind, sess),
				Detail: "atlas-cli init binds the folder again (another organisation or Project)."})
		}
		if p.ScopeRoot != p.Root {
			sec.Items = append(sec.Items, settings.Item{Label: "Memory lives at", Value: p.ScopeRoot})
		}
	}
	capture := settings.Item{Key: "capture", Label: "Session capture", Kind: settings.Toggle, On: bind != nil && bind.Enabled,
		Detail: "Record prompts, replies, tool calls and file changes of every agent session here. Pausing keeps what was recorded and still syncs it."}
	if bind == nil {
		capture.Disabled = needInit
	}
	sec.Items = append(sec.Items, capture,
		settings.Item{Key: "memory", Label: "Shared memory", Kind: settings.Toggle, On: project.MemorySharing(dir),
			Detail: "Agents read and write what every session learned in this repository, through the atlas_memory MCP server. Nothing is injected into prompts."})
	return sec
}

func (b *settingsBackend) agents(p *project.Project) settings.Section {
	sec := settings.Section{Title: "Agents", Icon: "◆"}
	for _, a := range hooks.All() {
		it := settings.Item{Key: "agent:" + a.ID(), Label: a.Name(), Kind: settings.Toggle}
		var notes []string
		if _, err := exec.LookPath(a.Binary()); err == nil {
			notes = append(notes, a.Binary()+" is on PATH.")
		} else {
			notes = append(notes, a.Binary()+" is not on PATH here.")
		}
		if p == nil {
			it.Disabled = needInit
		} else {
			switch a.State(p.Root) {
			case hooks.Current:
				it.On = true
			case hooks.Outdated:
				it.On, it.Value = true, "hook outdated: toggle off and on"
			default:
				if !a.Detect(p.Root) {
					it.Value = "not detected"
				}
			}
			if mw, ok := mcpconfig.For(a.ID()); ok && it.On {
				if mw.State(p.Root) == hooks.Current {
					notes = append(notes, "Memory server entry in "+strings.TrimPrefix(mw.Path(p.Root), p.Root+"/")+".")
				} else if it.Value == "" {
					it.Value = "no memory entry"
				}
			}
		}
		if n, ok := a.(hooks.Noter); ok {
			notes = append(notes, n.AddNote())
		}
		it.Detail = "Capture " + a.Name() + " sessions run in its own UI, and give it shared memory. " + strings.Join(notes, " ")
		sec.Items = append(sec.Items, it)
	}
	all := settings.Item{Key: "agents:all", Label: "Add all detected agents", Kind: settings.Action,
		Detail: "Hook every agent found on this machine or configured in this folder."}
	if p == nil {
		all.Disabled = needInit
	}
	sec.Items = append(sec.Items, all)
	return sec
}

func (b *settingsBackend) sync(ctx context.Context, p *project.Project) settings.Section {
	sec := settings.Section{Title: "Sync", Icon: "⇅"}
	if p == nil {
		sec.Items = []settings.Item{{Key: "sync", Label: "Background sync", Kind: settings.Toggle, Disabled: needInit}}
		return sec
	}
	on, line := syncState(p.Root)
	sec.Items = append(sec.Items, settings.Item{Key: "sync", Label: "Background sync", Kind: settings.Toggle, On: on, Value: line,
		Detail: "Keeps atlas-cli serve running for this Project: hooks hand it each prompt, tool call and reply, and it sends them to the cloud at once. Your agents' hooks restart it after a reboot. Same as atlas-cli sync on|off."})
	if st, err := store.Open(p.Root, false); err == nil {
		if c, err := st.OutboxCounts(ctx); err == nil {
			sec.Items = append(sec.Items, settings.Item{Label: "Outbox", Value: fmt.Sprintf("%d pending, %d failed", c[store.SyncPending], c[store.SyncFailed])})
		}
		_ = st.Close()
	}
	sec.Items = append(sec.Items,
		settings.Item{Key: "sync:now", Label: "Sync now", Kind: settings.Action, Detail: "Send what is pending to the cloud Project now (atlas-cli capture sync)."},
		settings.Item{Key: "sync:retry", Label: "Retry failed", Kind: settings.Action, Detail: "Re-queue rows that failed to sync, then sync (atlas-cli capture retry)."},
	)
	if e := strings.TrimSpace(hooks.LastError(p.Root)); e != "" {
		sec.Items = append(sec.Items, settings.Item{Label: "Last hook error", Value: e, Detail: e})
	}
	return sec
}

// model is the local MiniLM model memory search runs on.
func (b *settingsBackend) model(p *project.Project) settings.Section {
	sec := settings.Section{Title: "Model"}
	have := embed.Available()
	state, search := "not downloaded", "keywords only"
	if have {
		state = fmt.Sprintf("downloaded · %.0f MB", float64(embed.DiskSize())/1e6)
		search = "meaning and keywords"
	}
	sec.Items = []settings.Item{
		{Label: "Memory model", Value: embed.ModelID + " · " + state,
			Detail: "A small local model (MiniLM, 384 dimensions) that turns memories and documents into vectors, so memory_search finds them by meaning. It runs on this machine; nothing is sent anywhere."},
		{Label: "Memory search", Value: search},
		{Label: "Location", Value: embed.ModelDir(), Detail: "The Atlas app keeps its models here too: one download serves both."},
	}
	dl := settings.Item{Key: "model:download", Label: "Download model", Kind: settings.Action, Value: embed.Size,
		Detail: "Download " + embed.Repo + " (" + embed.Size + ", from Hugging Face, once), then embed this Project's memories and index its documents."}
	if have {
		dl.Disabled = "The model is already downloaded."
	}
	sec.Items = append(sec.Items, dl)
	idx := settings.Item{Key: "memory:index", Label: "Build memory index", Kind: settings.Action,
		Detail: "Embed every memory and index the project's documents now (atlas-cli memory index). Hooks and atlas-cli serve also keep it current."}
	if p == nil {
		idx.Disabled = needInit
	}
	sec.Items = append(sec.Items, idx)
	if have {
		sec.Items = append(sec.Items, settings.Item{Key: "model:remove", Label: "Remove model", Kind: settings.Action,
			Confirm: "This deletes the model files the Atlas app also uses.",
			Detail:  "Delete the model files to free " + embed.Size + ". Memory search falls back to keywords. The Atlas app uses the same files and would download them again."})
	}
	return sec
}

func (b *settingsBackend) about(p *project.Project) settings.Section {
	sec := settings.Section{Title: "About", Icon: "◇", Items: []settings.Item{
		{Label: "Version", Value: version},
		{Key: "update:check", Label: "Check for updates", Kind: settings.Action, Detail: "Ask GitHub for the latest release (atlas-cli update --check)."},
		{Key: "update", Label: "Update atlas", Kind: settings.Action,
			Detail: "Download the latest release for this machine, verify it against the release's checksums.txt, and replace this binary (atlas-cli update). A background listener is restarted on the new version."},
	}}
	if p != nil {
		sec.Items = append(sec.Items,
			settings.Item{Label: "CLI state", Value: clistate.Dir(p.Root), Detail: "Hook state, logs and the listener's run file; .atlas/ holds only what the app writes."},
			settings.Item{Label: "Hooks log", Value: hooks.LogPath(p.Root)},
		)
	}
	return sec
}

// ApplyProgress runs the long actions with a progress report: the model
// download and the index; everything else is Apply.
func (b *settingsBackend) ApplyProgress(ctx context.Context, key string, on bool, report func(float64, string)) (string, error) {
	p := b.project(ctx)
	switch key {
	case "model:download":
		if err := embed.Download(ctx, func(done, total int64) {
			if total > 0 {
				report(0.9*float64(done)/float64(total), fmt.Sprintf("%.0f of %.0f MB", float64(done)/1e6, float64(total)/1e6))
			}
		}); err != nil {
			return "", fmt.Errorf("download the model: %w", err)
		}
		msg := "Model downloaded: memory search now uses meaning and keywords."
		if p != nil {
			report(0.95, "embedding memories")
			if m, err := indexMemory(ctx, p.Root, p.ScopeRoot, false, func(string) {}, func(float64) {}); err == nil {
				msg += " " + m
			}
		}
		report(1, "")
		return msg, nil
	case "memory:index":
		if p == nil {
			return "", errNoProject
		}
		return indexMemory(ctx, p.Root, p.ScopeRoot, true, func(s string) { report(-1, s) }, func(f float64) { report(f, "") })
	}
	return b.Apply(ctx, key, on)
}

// Apply runs the command behind a row and returns its last line.
func (b *settingsBackend) Apply(ctx context.Context, key string, on bool) (string, error) {
	p := b.project(ctx)
	switch {
	case key == "model:remove":
		if err := embed.Remove(); err != nil {
			return "", err
		}
		return "Model removed: memory search uses keywords until it is downloaded again.", nil
	case key == "update:check":
		return applyUpdate(ctx, func(string, string) {}, "", true)
	case key == "update":
		return applyUpdate(ctx, func(string, string) {}, "", false)
	case key == "capture":
		return quietly(ctx, runCaptureGate(on))
	case key == "memory":
		return quietly(ctx, runMemoryGate(on))
	case p == nil:
		return "", errNoProject
	case key == "sync":
		return setBackgroundSync(p.Root, on)
	case key == "memory:index":
		return indexMemory(ctx, p.Root, p.ScopeRoot, true, func(string) {}, func(float64) {})
	case key == "sync:now":
		return quietly(ctx, runCaptureSync(false))
	case key == "sync:retry":
		return quietly(ctx, runCaptureSync(true))
	case key == "agents:all":
		var detected []hooks.Agent
		for _, a := range hooks.All() {
			if a.Detect(p.Root) {
				detected = append(detected, a)
			}
		}
		if len(detected) == 0 {
			return "No supported agent was found on this machine.", nil
		}
		return quietly(ctx, func(c *cobra.Command, _ []string) error {
			if err := installAgents(ctx, c.OutOrStdout(), p.Root, detected, false, false); err != nil {
				return err
			}
			fmt.Fprintf(c.OutOrStdout(), "Hooked %d agent(s).\n", len(detected))
			return nil
		})
	case strings.HasPrefix(key, "agent:"):
		a, ok := hooks.Get(strings.TrimPrefix(key, "agent:"))
		if !ok {
			return "", fmt.Errorf("unknown agent %s", key)
		}
		return quietly(ctx, func(c *cobra.Command, _ []string) error {
			if on {
				return installAgents(ctx, c.OutOrStdout(), p.Root, []hooks.Agent{a}, true, false)
			}
			return removeAgents(ctx, c.OutOrStdout(), p.Root, []hooks.Agent{a})
		})
	}
	return "", fmt.Errorf("unknown setting %s", key)
}

// quietly runs a command's body with its output captured, and returns the
// most telling line: the first one that is not a note.
func quietly(ctx context.Context, run func(*cobra.Command, []string) error) (string, error) {
	var buf bytes.Buffer
	c := &cobra.Command{}
	c.SetOut(&buf)
	c.SetErr(&buf)
	c.SetContext(ctx)
	err := run(c, nil)
	var last string
	for _, l := range strings.Split(buf.String(), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "note:") {
			last = l
		}
	}
	return last, err
}
