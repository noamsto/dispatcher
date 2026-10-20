package followups_test

import (
	"strings"
	"testing"

	"github.com/noamsto/dispatcher/crew/internal/followups"
)

const ledger = "<details><summary>Agent ledger</summary>\n\n- AC1 pass\n  keep  \n\n</details>\n"

func TestRewrite(t *testing.T) {
	links := []followups.Link{{N: 12, Title: "tidy guard"}, {N: 5, Title: "old thing", Note: "reopened"}}
	rendered := "- #12 — tidy guard\n- #5 — old thing (reopened)\n"
	tests := []struct {
		name, body, want string
		links            []followups.Link
	}{
		{
			name:  "untracked only, ledger after",
			body:  "## Summary\n\nx\n\n## Follow-ups (untracked)\n\n- raw one\n- raw two\n\n" + ledger,
			links: links,
			want:  "## Summary\n\nx\n\n## Follow-ups\n" + rendered + "\n" + ledger,
		},
		{
			name:  "untracked only, next heading",
			body:  "## Follow-ups (untracked)\n- raw\n## Notes\nn\n",
			links: links,
			want:  "## Follow-ups\n" + rendered + "\n## Notes\nn\n",
		},
		{
			name:  "untracked at EOF",
			body:  "intro\n\n## Follow-ups (untracked)\n- raw\n",
			links: links,
			want:  "intro\n\n## Follow-ups\n" + rendered,
		},
		{
			name:  "tracked exists, appended without duplicates",
			body:  "## Follow-ups\n\n- #12 — tidy guard\n\n## Notes\nn\n",
			links: links,
			want:  "## Follow-ups\n\n- #12 — tidy guard\n- #5 — old thing (reopened)\n\n## Notes\nn\n",
		},
		{
			name:  "#1 does not hide #12",
			body:  "## Follow-ups\n- #123 — other\n",
			links: []followups.Link{{N: 12, Title: "t"}},
			want:  "## Follow-ups\n- #123 — other\n- #12 — t\n",
		},
		{
			name:  "both present, untracked removed",
			body:  "## Follow-ups\n- #12 — tidy guard\n\n## Follow-ups (untracked)\n- raw\n\n" + ledger,
			links: links,
			want:  "## Follow-ups\n- #12 — tidy guard\n- #5 — old thing (reopened)\n\n" + ledger,
		},
		{
			name:  "neither, inserted before ledger",
			body:  "## Summary\n\nx\n\n" + ledger,
			links: links,
			want:  "## Summary\n\nx\n\n## Follow-ups\n" + rendered + "\n" + ledger,
		},
		{
			name:  "neither, appended at EOF",
			body:  "## Summary\n\nx\n",
			links: links,
			want:  "## Summary\n\nx\n\n## Follow-ups\n" + rendered,
		},
		{
			name:  "neither, EOF without newline",
			body:  "x",
			links: links[:1],
			want:  "x\n\n## Follow-ups\n- #12 — tidy guard",
		},
		{
			name:  "no links leaves the body alone",
			body:  "## Follow-ups (untracked)\n- raw\n",
			links: nil,
			want:  "## Follow-ups (untracked)\n- raw\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := followups.Rewrite(tt.body, tt.links); got != tt.want {
				t.Errorf("Rewrite =\n%q\nwant\n%q", got, tt.want)
			}
		})
	}
}

func TestRewriteCRLF(t *testing.T) {
	led := "<details><summary>Agent ledger</summary>\r\n\r\n- AC1 pass\r\n\r\n</details>\r\n"
	body := "## Summary\r\n\r\nx\r\n\r\n## Follow-ups (untracked)\r\n\r\n- raw\r\n\r\n" + led
	got := followups.Rewrite(body, []followups.Link{{N: 12, Title: "tidy guard"}})
	want := "## Summary\r\n\r\nx\r\n\r\n## Follow-ups\r\n- #12 — tidy guard\r\n\r\n" + led
	if got != want {
		t.Errorf("Rewrite =\n%q\nwant\n%q", got, want)
	}
	if !strings.HasSuffix(got, led) {
		t.Error("ledger bytes changed")
	}
}
