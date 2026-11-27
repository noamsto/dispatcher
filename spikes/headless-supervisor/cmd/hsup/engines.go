package main

import (
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/acp"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/claude"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/pi"
	"github.com/noamsto/dispatcher/spikes/headless-supervisor/internal/supervisor"
)

func registry() supervisor.Registry {
	return supervisor.Registry{
		"claude":     claude.New,
		"pi":         pi.New,
		"pi-acp":     acp.NewPiBolt,
		"claude-acp": acp.NewClaude,
	}
}
