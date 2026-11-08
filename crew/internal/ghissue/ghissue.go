// Package ghissue is the issue-filing half of the gh seam: cached views,
// creation with labels, reopen and comment, and the duplicate-candidate search
// that `crew followups` and the retro sweep share.
package ghissue

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/gh"
)

// Client reads and writes issues through GH. It caches View for its lifetime.
type Client struct {
	Ctx context.Context
	GH  gh.Client

	views map[int]*View
}

// New returns a Client with an empty view cache.
func New(ctx context.Context, c gh.Client) *Client {
	return &Client{Ctx: ctx, GH: c, views: map[int]*View{}}
}

// Comment is one issue comment.
type Comment struct {
	Body      string `json:"body"`
	CreatedAt string `json:"createdAt"`
}

// View is `gh issue view` with the fields the callers read.
type View struct {
	State       string    `json:"state"`
	StateReason string    `json:"stateReason"`
	Title       string    `json:"title"`
	Body        string    `json:"body"`
	CreatedAt   string    `json:"createdAt"`
	ClosedAt    string    `json:"closedAt"`
	Comments    []Comment `json:"comments"`
}

// Issue is one row of `gh issue list`.
type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
}

// Read runs gh and decodes its JSON output into dst.
func (c *Client) Read(dst any, args ...string) error {
	out, err := c.GH.Run(c.Ctx, args...)
	if err != nil {
		return fmt.Errorf("gh %s failed: %v", strings.Join(args[:2], " "), err)
	}
	if err := json.Unmarshal([]byte(out), dst); err != nil {
		return fmt.Errorf("unreadable gh %s output: %v", strings.Join(args[:2], " "), err)
	}
	return nil
}

// View is `gh issue view`, cached for the Client's lifetime. Callers may
// update the returned value to mirror a write they made.
func (c *Client) View(n int) (*View, error) {
	if v, ok := c.views[n]; ok {
		return v, nil
	}
	v := &View{}
	if err := c.Read(v, "issue", "view", strconv.Itoa(n), "--json", "state,stateReason,title,comments,body,createdAt,closedAt"); err != nil {
		return nil, err
	}
	c.views[n] = v
	return v, nil
}

var issueNumberRe = regexp.MustCompile(`/(\d+)$`)

// Create files an issue assigned to the caller, with one --label per label.
func (c *Client) Create(title, body string, labels ...string) (int, error) {
	args := []string{"issue", "create", "--title", title, "--body", body, "--assignee", "@me"}
	for _, l := range labels {
		args = append(args, "--label", l)
	}
	out, err := c.GH.Run(c.Ctx, args...)
	if err != nil {
		return 0, fmt.Errorf("gh issue create %q failed: %v", title, err)
	}
	m := issueNumberRe.FindStringSubmatch(strings.TrimSpace(out))
	if m == nil {
		return 0, fmt.Errorf("gh issue create %q printed no issue URL: %q", title, out)
	}
	return strconv.Atoi(m[1])
}

// Reopen reopens issue n, with a comment unless comment is empty.
func (c *Client) Reopen(n int, comment string) error {
	args := []string{"issue", "reopen", strconv.Itoa(n)}
	if comment != "" {
		args = append(args, "--comment", comment)
	}
	_, err := c.GH.Run(c.Ctx, args...)
	return err
}

// Comment posts body on issue n.
func (c *Client) Comment(n int, body string) error {
	_, err := c.GH.Run(c.Ctx, "issue", "comment", strconv.Itoa(n), "--body", body)
	return err
}

// Candidates are the open issues a search for title returns, for the human to
// judge as duplicates.
func (c *Client) Candidates(title string) ([]Issue, error) {
	var is []Issue
	err := c.Read(&is, "issue", "list", "--state", "open", "--search", title, "--limit", "5", "--json", "number,title")
	return is, err
}

// FirstLine is s up to its first line break.
func FirstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return strings.TrimSuffix(l, "\r")
}

// Stamp reads one of gh's RFC 3339 timestamps; the zero time means unreadable.
func Stamp(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// FormatCandidates is "#N title, #M title".
func FormatCandidates(is []Issue) string {
	parts := make([]string, len(is))
	for i, c := range is {
		parts[i] = fmt.Sprintf("#%d %s", c.Number, c.Title)
	}
	return strings.Join(parts, ", ")
}
