package rosterrender

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/dispatcher/crew/internal/identity"
)

// defaultRosterDir is the arm's `${CREW_ROSTER_DIR:-…}`: the directory the aeye
// carousel watches, overridable so a test never writes beside a live one.
const defaultRosterDir = "/tmp/claude-status/images/diagrams/src"

// plainEpoch is the arm's `^[1-9][0-9]{0,11}$`: a crew id's prefix that reads as
// epoch seconds, with no leading zero and at most 12 digits.
var plainEpoch = regexp.MustCompile(`\A[1-9][0-9]{0,11}\z`)

// target is `_rr_target`: one diagram per crew per repo bus,
// `roster-<repo>-<when>-<h4>.d2`. `<repo>` is the dir that owns the bus, `<when>`
// the crew id's epoch prefix in the renderer's local timezone (the crew id's own
// sanitized text when the prefix is not a plain epoch), and `<h4>` a 16-bit
// checksum of the bus path plus the crew, so two repos' renderers, or two crews
// started in the same minute, collide only with negligible probability.
func target(common, crew, rosterDir string, now func() time.Time) string {
	if rosterDir == "" {
		rosterDir = defaultRosterDir
	}
	when := crew
	if prefix, _, found := strings.Cut(crew, "-"); found && plainEpoch.MatchString(prefix) {
		if secs, err := strconv.ParseInt(prefix, 10, 64); err == nil {
			when = time.Unix(secs, 0).In(now().Location()).Format("01-02-1504")
		}
	}
	sum := identity.CKsum([]byte(common+"|"+crew)) % 65536
	return fmt.Sprintf("%s/roster-%s-%s-%04x.d2", rosterDir,
		sanitize(filepath.Base(filepath.Dir(common))), sanitize(when), sum)
}

// sanitize is `tr -c 'A-Za-z0-9._-' '_'`: every byte outside the set becomes an
// underscore, so neither a repo dir nor a crew id can ever steer a path.
func sanitize(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}
