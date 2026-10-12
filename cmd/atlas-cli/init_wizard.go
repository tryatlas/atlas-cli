package main

import (
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/tryatlas/atlas-cli/internal/clistate"
	"github.com/tryatlas/atlas-cli/internal/embed"
	"github.com/tryatlas/atlas-cli/internal/hooks"
	"github.com/tryatlas/atlas-cli/internal/project"
	"github.com/tryatlas/atlas-cli/internal/tui"
)

// initStepper numbers atlas-cli init's steps: Organisation, Project, Agents and
// Background sync for a cloud Project; Agents only for a local one.
type initStepper []string

func initSteps(local bool) initStepper {
	s := initStepper{"Organisation", "Project", "Agents", "Background sync"}
	if local {
		s = initStepper{"Agents"}
	}
	if !embed.Available() {
		s = append(s, "Memory search")
	}
	return s
}

func (s initStepper) step(title string) tui.Step {
	for i, t := range s {
		if t == title {
			return tui.Step{N: i + 1, Total: len(s), Title: title}
		}
	}
	return tui.Step{Title: title}
}

// finishInit runs the steps after the binding: which agents to hook, and
// whether to keep syncing in the background.
func finishInit(cmd *cobra.Command, root string, local, guided bool, steps initStepper) error {
	out := cmd.OutOrStdout()
	agents, err := pickAgents(cmd, root, guided, steps.step("Agents"))
	if err != nil {
		return err
	}
	on, ask := false, false
	if !local {
		if on, ask, err = pickSync(cmd, root, guided, steps.step("Background sync")); err != nil {
			return err
		}
	}
	model := false
	if guided && !embed.Available() && project.MemorySharing(project.ScopeRoot(cmd.Context(), root)) {
		i, err := chooseStep(steps.step("Memory search"), "Download the memory search model? Agents then find memories by meaning, not only by words.", []tui.Choice{
			{Label: "Download", Detail: embed.Size + ", once; the same model the Atlas app uses"},
			{Label: "Not now", Detail: "keyword search; atlas-cli memory index downloads it later"},
		}, 0)
		if err != nil {
			return err
		}
		model = i == 0
	}
	if !guided {
		if len(agents) > 0 {
			if err := installAgents(cmd.Context(), out, root, agents, false, false); err != nil {
				return err
			}
		}
		if ask {
			msg, err := setBackgroundSync(root, on)
			if err != nil {
				return err
			}
			fmt.Fprintln(out, msg)
		}
		return nil
	}
	// On a terminal the work runs under the progress bar, each line a step.
	if model {
		fmt.Fprintln(out)
		scope := project.ScopeRoot(cmd.Context(), root)
		if err := tui.WorkProgress(out, "Setting up memory search", func(step tui.StepFunc, frac tui.FracFunc) (string, error) {
			return indexMemory(cmd.Context(), root, scope, true, step, frac)
		}); err != nil {
			return err
		}
	} else if embed.Available() {
		spawnIndex(root) // the index, in the background
	}
	if len(agents) > 0 || ask {
		fmt.Fprintln(out)
		err := tui.Work(out, "Initializing Atlas in "+root, func(step tui.StepFunc) (string, error) {
			w := tui.Lines(io.Discard, func(l string) string {
				if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "note:") {
					step(l)
				}
				return l
			})
			if len(agents) > 0 {
				if err := installAgents(cmd.Context(), w, root, agents, false, false); err != nil {
					return "", err
				}
			}
			if ask {
				msg, err := setBackgroundSync(root, on)
				if err != nil {
					return "", err
				}
				step(msg)
			}
			return "Atlas is ready in " + root, nil
		})
		if err != nil {
			return err
		}
		for _, a := range agents {
			if n, ok := a.(hooks.Noter); ok {
				tui.Note(out, "↳ "+n.AddNote())
			}
		}
	} else {
		tui.Ready(out, "Atlas is ready in "+root)
	}
	fmt.Fprintln(out)
	tui.Note(out, "Use your agents as usual; sessions and memory are captured as they run.")
	tui.Note(out, "atlas-cli status shows what was captured · atlas-cli settings changes any of this")
	return nil
}

// detail says what the machine has of an agent.
func agentDetail(a hooks.Agent, root string) string {
	d := "configured here"
	if _, err := exec.LookPath(a.Binary()); err == nil {
		d = a.Binary() + " on PATH"
	}
	switch a.State(root) {
	case hooks.Current:
		d += " · already hooked"
	case hooks.Outdated:
		d += " · hook outdated"
	}
	return d
}

// pickAgents is step 3: --agents or --all-agents, else a checklist of the
// agents detected here with a row that selects them all.
func pickAgents(cmd *cobra.Command, root string, guided bool, step tui.Step) ([]hooks.Agent, error) {
	flags := cmd.Flags()
	ids, _ := flags.GetStringSlice("agents")
	all, _ := flags.GetBool("all-agents")
	var detected []hooks.Agent
	for _, a := range hooks.All() {
		if a.Detect(root) {
			detected = append(detected, a)
		}
	}
	switch {
	case len(ids) > 0:
		return resolveAgents(ids, false, nil)
	case all:
		if len(detected) == 0 {
			fmt.Fprintln(cmd.OutOrStdout(), "No supported agent was found on this machine (atlas-cli agents list).")
		}
		return detected, nil
	case !guided:
		return nil, nil
	case len(detected) == 0:
		tui.StepDone(cmd.OutOrStdout(), step, "none found on this machine (atlas-cli agents add <agent> later)")
		return nil, nil
	}
	choices := make([]tui.Choice, len(detected))
	checked := make([]bool, len(detected))
	for i, a := range detected {
		choices[i] = tui.Choice{Label: a.Name(), Detail: agentDetail(a, root)}
		checked[i] = true
	}
	picked, err := chooseMany(step, "Which agents should Atlas capture and give shared memory?", "All agents", choices, checked)
	if err != nil {
		return nil, err
	}
	out := make([]hooks.Agent, len(picked))
	for i, n := range picked {
		out[i] = detected[n]
	}
	return out, nil
}

// pickSync is step 4: background sync on or off. ask is false when nothing
// was asked and no flag given, so the current choice stays.
func pickSync(cmd *cobra.Command, root string, guided bool, step tui.Step) (on, ask bool, err error) {
	switch v, _ := cmd.Flags().GetString("sync"); v {
	case "on":
		return true, true, nil
	case "off":
		return false, true, nil
	case "":
	default:
		return false, false, fmt.Errorf("--sync must be on or off, not %q", v)
	}
	if !guided {
		return false, false, nil
	}
	initial := 1
	if clistate.LoadPrefs(root).BackgroundSync {
		initial = 0
	}
	i, err := chooseStep(step, "Keep syncing in the background? (atlas-cli sync on|off changes it later)", []tui.Choice{
		{Label: "On", Detail: "a background atlas-cli serve sends prompts, tool calls and replies as they happen"},
		{Label: "Off", Detail: "each agent hook syncs on its own; nothing keeps running"},
	}, initial)
	if err != nil {
		return false, false, err
	}
	return i == 0, true, nil
}
