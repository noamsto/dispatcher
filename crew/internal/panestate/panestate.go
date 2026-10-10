// Package panestate is the status mirror on a pane's border: the three
// `@crew_*` pane options `_publish_pane_state` set, shared by `stall` and
// `status` so Go keeps one copy.
package panestate

// detailRunes is the contract's width for the border detail.
const detailRunes = 40

// Publish sets @crew_state, @crew_detail (cut to 40 characters) and
// @crew_source, in that order, through set.
func Publish(set func(option, value string), state, detail, source string) {
	if r := []rune(detail); len(r) > detailRunes {
		detail = string(r[:detailRunes])
	}
	set("@crew_state", state)
	set("@crew_detail", detail)
	set("@crew_source", source)
}
