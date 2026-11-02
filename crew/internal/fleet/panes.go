package fleet

import (
	"context"

	"github.com/noamsto/dispatcher/crew/internal/frame"
	"github.com/noamsto/dispatcher/crew/internal/tmux"
)

// UnsentInput reports whether a claude pane holds a draft typed and not sent.
// The capture is colored (`-e`): a plain one strips the dim attribute that
// tells claude's prompt-suggestion ghost text from typed input.
func UnsentInput(ctx context.Context, c tmux.Client, pane string) bool {
	plain := tmux.Output(ctx, c, "capture-pane", "-p", "-t", pane)
	colored := tmux.Output(ctx, c, "capture-pane", "-e", "-p", "-t", pane)
	return frame.ClaudeUnsentInput(plain, colored)
}
