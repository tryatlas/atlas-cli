// Package procs reads process ancestry, used to tell which agent process a
// hook or an MCP bridge belongs to (ADR-0003: joining memory and capture
// sessions).
package procs

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Parent returns a process's parent pid, or 0 when it cannot be read.
func Parent(pid int) int {
	if pid <= 1 {
		return 0
	}
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil { // Linux
		// pid (comm) state ppid …; comm may contain spaces, so cut after the last ')'.
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 {
			if f := strings.Fields(s[i+1:]); len(f) >= 2 {
				if p, err := strconv.Atoi(f[1]); err == nil {
					return p
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-o", "ppid=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // G204: a pid
	if err != nil {
		return 0
	}
	p, _ := strconv.Atoi(strings.TrimSpace(string(out)))
	return p
}

// Ancestors returns up to n ancestors of pid, nearest first.
func Ancestors(pid, n int) []int {
	var out []int
	for i := 0; i < n; i++ {
		pid = Parent(pid)
		if pid <= 1 {
			break
		}
		out = append(out, pid)
	}
	return out
}

// Format writes pids as "1,2,3"; Parse reads them back.
func Format(pids []int) string {
	s := make([]string, len(pids))
	for i, p := range pids {
		s[i] = strconv.Itoa(p)
	}
	return strings.Join(s, ",")
}

// Parse reads "1,2,3".
func Parse(s string) []int {
	var out []int
	for _, f := range strings.Split(s, ",") {
		if p, err := strconv.Atoi(strings.TrimSpace(f)); err == nil && p > 1 {
			out = append(out, p)
		}
	}
	return out
}

// NearestCommon returns the first pid of a that also appears in b.
func NearestCommon(a, b []int) (int, bool) {
	in := map[int]bool{}
	for _, p := range b {
		in[p] = true
	}
	for _, p := range a {
		if in[p] {
			return p, true
		}
	}
	return 0, false
}
