package ui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

type orderWriter struct {
	buf *bytes.Buffer
	log *[]string
}

func (w orderWriter) Write(p []byte) (int, error) {
	*w.log = append(*w.log, "write:"+strings.ReplaceAll(string(p), "\x1b", "ESC"))
	return w.buf.Write(p)
}

func TestThemeAwareExecBracketsEditor(t *testing.T) {
	var log []string
	w := orderWriter{buf: &bytes.Buffer{}, log: &log}
	inner := func(_ *exec.Cmd, cb tea.ExecCallback) tea.Cmd {
		return func() tea.Msg {
			log = append(log, "editor")
			return cb(nil)
		}
	}
	type done struct{}
	cmd := themeAwareExec(inner, w)(exec.Command("vim"), func(error) tea.Msg {
		log = append(log, "callback")
		return done{}
	})
	if msg := cmd(); msg != (done{}) {
		t.Fatalf("msg = %v", msg)
	}
	want := []string{
		"write:ESC[?2031l",
		"editor",
		"write:ESC[?2031hESC[?996n",
		"callback",
	}
	if strings.Join(log, "|") != strings.Join(want, "|") {
		t.Fatalf("order = %v, want %v", log, want)
	}
}
