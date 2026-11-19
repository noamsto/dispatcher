package stream

import "io"

// runFollow is `--follow`; the follow step replaces this stub.
func runFollow(_ FleetRequest, _ call, _ string, stderr io.Writer, _ FleetOptions) int {
	say(stderr, "crew: fleet stream: --follow is not implemented\n")
	return exitUsage
}
