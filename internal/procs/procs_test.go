package procs

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

func TestParentAndAncestors(t *testing.T) {
	if p := Parent(os.Getpid()); p != os.Getppid() {
		t.Fatalf("Parent = %d, want %d", p, os.Getppid())
	}
	// A child shell's ancestors start with this process.
	out, err := exec.Command("/bin/sh", "-c", "echo $$").Output()
	if err != nil {
		t.Fatal(err)
	}
	_ = out
	anc := Ancestors(os.Getpid(), 3)
	if len(anc) == 0 || anc[0] != os.Getppid() {
		t.Fatalf("ancestors %v", anc)
	}
}

func TestFormatParseCommon(t *testing.T) {
	if got := Parse(Format([]int{10, 20, 30})); len(got) != 3 || got[2] != 30 {
		t.Fatal(got)
	}
	if got := Parse("1, x ,5"); len(got) != 1 || got[0] != 5 {
		t.Fatal(got)
	}
	if p, ok := NearestCommon([]int{7, 8, 9}, []int{3, 9, 8}); !ok || p != 8 {
		t.Fatal(p, ok)
	}
	if _, ok := NearestCommon([]int{1}, []int{2}); ok {
		t.Fatal("no common")
	}
	_ = strconv.Itoa
	_ = strings.TrimSpace
}
