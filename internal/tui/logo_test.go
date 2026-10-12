package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

func TestLogoShapes(t *testing.T) {
	if s := ansiRE.ReplaceAllString(Logo(0), ""); strings.Contains(s, "●") {
		t.Fatal("nothing lit at 0")
	}
	// Array Bold's "/" three times: 4 dots wide, 3 apart, one column right
	// per row, 7 rows of two dot rows each.
	logo := strings.Split(ansiRE.ReplaceAllString(Logo(1), ""), "\n")
	if len(logo) != 7 || logo[0] != "      ●●●●   ●●●●   ●●●●" || logo[6] != "●●●●   ●●●●   ●●●●      " {
		t.Fatalf("%q", logo)
	}
	if got := ansiRE.ReplaceAllString(pulse(0), ""); got != "▟▛ ▟▛ ▟▛" {
		t.Fatal(got)
	}
}

func TestLightDetection(t *testing.T) {
	for _, c := range []struct {
		theme, fgbg string
		want        bool
	}{{"", "", false}, {"light", "", true}, {"dark", "0;15", false}, {"", "0;15", true}, {"", "15;0", false}, {"", "0;7", true}} {
		if got := light(c.theme, c.fgbg); got != c.want {
			t.Errorf("light(%q, %q) = %v", c.theme, c.fgbg, got)
		}
	}
}

func TestLoginScreen(t *testing.T) {
	var m tea.Model = loginModel{}
	if s := ansiRE.ReplaceAllString(m.View().Content, ""); !strings.Contains(s, "Asking Atlas for a sign-in code") {
		t.Fatal(s)
	}
	m, _ = m.Update(loginPrompt{URL: "https://auth.example/device", Code: "WDJB-MJHT", Expires: time.Now().Add(10 * time.Minute)})
	s := ansiRE.ReplaceAllString(m.View().Content, "")
	for _, want := range []string{"Atlas", "sign in", "YOUR CODE", "W D J B - M J H T", "https://auth.example/device", "Waiting for approval", "code expires in"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %q:\n%s", want, s)
		}
	}
	m, _ = m.Update(loginDone{who: "Ada <ada@example.com>"})
	if s := ansiRE.ReplaceAllString(m.View().Content, ""); !strings.Contains(s, "✓ Signed in as Ada <ada@example.com>") {
		t.Fatal(s)
	}
}

func TestWorkScreen(t *testing.T) {
	var m tea.Model = workModel{title: "Syncing", start: time.Now()}
	m, _ = m.Update(workStep("10 row(s) sent"))
	s := ansiRE.ReplaceAllString(m.View().Content, "")
	if !strings.Contains(s, "Syncing") || !strings.Contains(s, "└ 10 row(s) sent") || !strings.Contains(s, "▮") || !strings.Contains(s, "▯") {
		t.Fatal(s)
	}
	m, _ = m.Update(workDone{msg: "Sent 10 row(s)."})
	if s := ansiRE.ReplaceAllString(m.View().Content, ""); strings.Contains(s, "▯") {
		t.Fatalf("the bar must show full before the result:\n%s", s)
	}
	m, _ = m.Update(workShown{})
	if s := ansiRE.ReplaceAllString(m.View().Content, ""); !strings.Contains(s, "├ 10 row(s) sent") || !strings.Contains(s, "✓ Sent 10 row(s).") {
		t.Fatal(s)
	}
}
