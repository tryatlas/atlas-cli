package acp

import (
	"context"
	"fmt"
	"os"
)

// Resolver turns an installed entry into a command on disk
// (evidence-acp.md §1.4): three resolvers, no fallback between them, no
// PATH search.
type Resolver struct {
	Data     string // data dir
	Node     *Node
	Archives *Archives
	Progress Progress
}

// NewResolver uses the data dir.
func NewResolver(progress Progress) (*Resolver, error) {
	d, err := DataDir()
	if err != nil {
		return nil, err
	}
	n, err := NewNode()
	if err != nil {
		return nil, err
	}
	return &Resolver{Data: d, Node: n, Archives: NewArchives(d), Progress: progress}, nil
}

// Resolve builds the launch command. reg may be nil for custom entries.
func (r *Resolver) Resolve(ctx context.Context, id string, inst *Installed, reg *Registry) (Command, error) {
	extraEnv, extraArgs := extra(id, hostLookup)
	switch inst.Type {
	case TypeCustom:
		path := inst.CommandPath()
		if path == "" {
			return Command{}, fmt.Errorf("%s: the installed entry has no command", id)
		}
		return Command{Path: path, Args: append(append([]string{}, inst.Args...), extraArgs...),
			Env: layer(extraEnv, inst.Env)}, nil
	case TypeRegistry:
	default:
		return Command{}, fmt.Errorf("%s: unknown installed type %q", id, inst.Type)
	}
	if reg == nil {
		return Command{}, fmt.Errorf("%s: %w", id, ErrNotInRegistry)
	}
	e, ok := reg.Find(id)
	if !ok {
		return Command{}, fmt.Errorf("%s: %w", id, ErrNotInRegistry)
	}
	switch e.KindOn(PlatformKey()) {
	case KindBinary:
		t := e.Distribution.Binary[PlatformKey()]
		path, err := r.Archives.Ensure(ctx, e.ID, e.Version, t, r.Node, r.Progress)
		if err != nil {
			return Command{}, err
		}
		env := layer(t.Env, extraEnv, inst.Env)
		if t.Cmd == "node" {
			env = layer(r.Node.CommandEnv(), env)
		}
		return Command{Path: path, Args: append(append([]string{}, t.Args...), extraArgs...), Env: env}, nil
	case KindNpx:
		d := *e.Distribution.Npx
		in, err := ResolveNpx(ctx, r.Node, r.Data, e.ID, d, r.Progress)
		if err != nil {
			return Command{}, err
		}
		env := layer(r.Node.CommandEnv(), d.Env, extraEnv, inst.Env)
		if in.Native {
			return Command{Path: in.Executable, Args: append(append([]string{}, d.Args...), extraArgs...), Env: env}, nil
		}
		args := append([]string{in.Executable}, d.Args...)
		return Command{Path: r.Node.Bin(), Args: append(args, extraArgs...), Env: env}, nil
	}
	return Command{}, fmt.Errorf("%s does not run on %s", id, PlatformKey())
}

// executable reports that path is a regular file.
func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
