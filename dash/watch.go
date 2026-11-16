package main

import (
	"context"
	"encoding/json"
	"io"
	"time"
)

type collectResult struct {
	v   any
	err error
}

// runWatch writes one compact JSON line from collect at start and one per
// debounced change, until ctx is done, changes closes, or a collect or write
// fails. A collect runs off the loop so changes keep draining while it is in
// flight (a remote roster can take its full timeout); those changes coalesce
// into exactly one follow-up collect.
func runWatch(ctx context.Context, out io.Writer, changes <-chan struct{}, collect func(context.Context) (any, error), debounce time.Duration) error {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)

	results := make(chan collectResult, 1)
	start := func() {
		go func() {
			v, err := collect(ctx)
			results <- collectResult{v, err}
		}()
	}

	start()
	inFlight, pending := true, false
	var debounced <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return nil
		case _, ok := <-changes:
			if !ok {
				return nil
			}
			if inFlight {
				pending = true
			} else if debounced == nil {
				debounced = time.After(debounce)
			}
		case <-debounced:
			debounced = nil
			inFlight = true
			start()
		case r := <-results:
			inFlight = false
			if r.err != nil {
				return r.err
			}
			if err := enc.Encode(r.v); err != nil {
				return err
			}
			if pending {
				pending = false
				debounced = time.After(debounce)
			}
		}
	}
}
