package main

import (
	"context"
	"os"
	"strings"

	"github.com/noamsto/dispatcher/dash/internal/data"
	"github.com/noamsto/dispatcher/dash/internal/view"
)

const defaultEventsLimit = 200

// eventsCollector serves --view events. The first call returns the tail (or
// the rows after --since); under --watch every later call returns only rows
// appended since the previous page and returns nil, skipping the emission,
// when there are none. One-shot pages honour --limit and report more.
func eventsCollector(path string, pathErr error, o opts) func(context.Context) (any, error) {
	host := shortHost()
	cursor := o.since
	first := true
	return func(context.Context) (any, error) {
		if pathErr != nil {
			return nil, pathErr
		}
		limit := o.limit
		switch {
		case o.watch && (!first || cursor != ""):
			limit = 0
		case limit == 0 && cursor == "":
			limit = defaultEventsLimit
		}
		page, err := data.ReadEvents(path, cursor, limit)
		if err != nil {
			return nil, err
		}
		wasFirst := first
		first, cursor = false, page.Cursor
		if o.watch && !wasFirst && len(page.Events) == 0 && !page.Reset {
			return nil, nil
		}
		return view.BuildEvents(host, page, resolveNow()), nil
	}
}

func shortHost() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	h, _, _ = strings.Cut(h, ".")
	return h
}
