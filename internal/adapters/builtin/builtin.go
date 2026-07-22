// Package builtin owns the compile-time provider registry used by the agent.
// Provider packages stay independent; the runtime only consumes this small
// descriptor surface and never needs to know how a provider stores metadata.
package builtin

import (
	"fmt"
	"sort"
	"strings"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/adapters/antigravity"
	"github.com/tokemon/tokemon/internal/adapters/claude"
	"github.com/tokemon/tokemon/internal/adapters/codex"
	"github.com/tokemon/tokemon/internal/adapters/copilot"
	"github.com/tokemon/tokemon/internal/adapters/generic"
	"github.com/tokemon/tokemon/internal/adapters/openclaw"
	"github.com/tokemon/tokemon/internal/adapters/opencode"
)

type Config struct {
	Home       string
	JSONLPaths []string
}

type Definition struct {
	ID             string
	DisplayName    string
	Version        string
	DefaultEnabled bool
	New            func(Config) adapters.Adapter
}

var definitions = []Definition{
	{ID: "claude-code", DisplayName: "Claude Code", Version: "0.2.0", DefaultEnabled: true, New: func(config Config) adapters.Adapter {
		return claude.New(config.Home)
	}},
	{ID: "codex", DisplayName: "Codex", Version: "0.6.0", DefaultEnabled: true, New: func(config Config) adapters.Adapter {
		return codex.New(config.Home)
	}},
	{ID: "copilot-cli", DisplayName: "GitHub Copilot CLI", Version: "0.1.0", DefaultEnabled: true, New: func(config Config) adapters.Adapter {
		return copilot.New(config.Home)
	}},
	{ID: "opencode", DisplayName: "OpenCode", Version: "0.2.0", DefaultEnabled: true, New: func(config Config) adapters.Adapter {
		return opencode.New(config.Home)
	}},
	{ID: "antigravity", DisplayName: "Antigravity", Version: "0.2.0", DefaultEnabled: true, New: func(config Config) adapters.Adapter {
		return antigravity.New(config.Home)
	}},
	{ID: "openclaw", DisplayName: "OpenClaw", Version: "0.1.0", DefaultEnabled: true, New: func(config Config) adapters.Adapter {
		return openclaw.New(config.Home)
	}},
	{ID: "generic-jsonl", DisplayName: "Generic JSONL", Version: "0.2.0", DefaultEnabled: false, New: func(config Config) adapters.Adapter {
		return generic.New(config.JSONLPaths...)
	}},
}

// Definitions returns a copy so callers cannot mutate the registry.
func Definitions() []Definition {
	return append([]Definition(nil), definitions...)
}

// Select chooses the built-in adapters in stable registry order. An empty
// selection preserves the historical default of enabling every native adapter;
// configured generic JSONL paths opt the generic adapter in automatically.
func Select(config Config, requested []string) ([]Definition, error) {
	byID := make(map[string]Definition, len(definitions))
	for _, definition := range definitions {
		byID[definition.ID] = definition
	}

	selected := make(map[string]struct{})
	if len(requested) == 0 {
		for _, definition := range definitions {
			if definition.DefaultEnabled || (definition.ID == "generic-jsonl" && len(config.JSONLPaths) > 0) {
				selected[definition.ID] = struct{}{}
			}
		}
	} else {
		for _, raw := range requested {
			id := strings.ToLower(strings.TrimSpace(raw))
			if id == "" {
				continue
			}
			if _, ok := byID[id]; !ok {
				return nil, fmt.Errorf("unknown adapter %q; choose from %s", raw, strings.Join(IDs(), ", "))
			}
			selected[id] = struct{}{}
		}
	}

	result := make([]Definition, 0, len(selected))
	for _, definition := range definitions {
		if _, ok := selected[definition.ID]; ok {
			result = append(result, definition)
		}
	}
	return result, nil
}

func Build(config Config, requested []string) ([]adapters.Adapter, []Definition, error) {
	selected, err := Select(config, requested)
	if err != nil {
		return nil, nil, err
	}
	result := make([]adapters.Adapter, 0, len(selected))
	for _, definition := range selected {
		result = append(result, definition.New(config))
	}
	return result, selected, nil
}

func IDs() []string {
	result := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, definition.ID)
	}
	sort.Strings(result)
	return result
}
