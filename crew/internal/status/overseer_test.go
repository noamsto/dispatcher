package status

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/sender"
)

func (f *fixture) overseerMsg(self, crew, body string) (int, string) {
	var stderr bytes.Buffer
	code := RunOverseerMsg(f.t.Context(), OverseerMsgRequest{Self: self, Crew: crew, Body: body}, f.paths, &stderr, f.opts())
	return code, stderr.String()
}

func TestOverseerMsgAppendsAnOverseerRow(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	body := `{"kind":"notice","detail":"x"}`
	if code, stderr := f.overseerMsg("h1", "c1", body); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(`{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:h1","to":"dispatcher:c1","kind":"msg","body":"{\"kind\":\"notice\",\"detail\":\"x\"}"}`)
}

func TestOverseerMsgTakesTheCrewFromTheRequest(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	if code, stderr := f.overseerMsg("h1", "other", "hi"); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	f.wantRow(`{"ts":` + tsText + `,"crew_id":"other","from":"overseer:h1","to":"dispatcher:other","kind":"msg","body":"hi"}`)
}

func TestOverseerMsgPostsARowAtTheLineMax(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	const bare = `{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:h1","to":"dispatcher:c1","kind":"msg","body":""}`
	body := strings.Repeat("x", 4096-len(bare))
	if code, stderr := f.overseerMsg("h1", "c1", body); code != 0 || stderr != "" {
		t.Fatalf("%d %q", code, stderr)
	}
	got := f.lines()
	if len(got) != 1 {
		t.Fatalf("log has %d rows, want 1", len(got))
	}
	if len(got[0]) != 4096 || strings.Contains(got[0], "elided") {
		t.Errorf("row is %d bytes, want an untouched 4096-byte row", len(got[0]))
	}
}

func TestOverseerMsgRefusesAnUnmarkedSession(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	delete(f.env, "CREW_OVERSEER")
	code, stderr := f.overseerMsg("h1", "c1", "hi")
	reason := sender.Check(t.Context(), "overseer:h1", "c1", f.opts().Sender)
	if reason == "" {
		t.Fatal("fixture does not trip the sender check")
	}
	if want := "crew: overseer msg: refusing sender 'overseer:h1' — " + reason + "\n"; code != 1 || stderr != want {
		t.Errorf("got %d %q, want 1 %q", code, stderr, want)
	}
	if _, err := os.Stat(f.paths.Dir); !os.IsNotExist(err) {
		t.Errorf("bus dir created on a refused sender: %v", err)
	}
	f.wantNoRow()
}

func TestOverseerMsgRefusesARowThatWouldBeElided(t *testing.T) {
	f := newFixture(t)
	f.overseer("h1")
	f.log()
	// Each \" in the body encodes to four bytes, so the 3400-byte body overflows the line.
	body := `{"detail":"` + strings.Repeat(`\"`, 1700) + `"}`
	code, stderr := f.overseerMsg("h1", "c1", body)
	line := `{"ts":` + tsText + `,"crew_id":"c1","from":"overseer:h1","to":"dispatcher:c1","kind":"msg","body":` + jsonStr(body) + `}`
	if len(line) <= 4096 {
		t.Fatalf("fixture row is %d bytes, not over the line", len(line))
	}
	want := fmt.Sprintf("crew: overseer msg: the encoded row is %d bytes, over the 4096-byte bus line, so the body would be elided\n", len(line))
	if code != 1 || stderr != want {
		t.Errorf("got %d %q, want 1 %q", code, stderr, want)
	}
	f.wantNoRow()
}
