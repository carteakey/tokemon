package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	AliasKindModel   = "model"
	AliasKindMachine = "machine"
)

type DisplayAlias struct {
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Alias    string `json:"alias"`
}

// DisplayAliases returns the model and machine display names. The rows are
// tiny, but the read still needs the store's single connection, which a
// background analytics refresh can hold for a long query. Caching them keeps a
// warm page render free of database work, so it never queues behind a refresh.
// SetDisplayAlias is the only writer and drops the cache.
func (s *Store) DisplayAliases(ctx context.Context) ([]DisplayAlias, error) {
	s.aliasesMu.Lock()
	defer s.aliasesMu.Unlock()
	if s.aliasesCached {
		return s.aliasesCache, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT kind, identity, alias FROM display_aliases ORDER BY kind, identity`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var aliases []DisplayAlias
	for rows.Next() {
		var alias DisplayAlias
		if err := rows.Scan(&alias.Kind, &alias.Identity, &alias.Alias); err != nil {
			return nil, err
		}
		aliases = append(aliases, alias)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	s.aliasesCache = aliases
	s.aliasesCached = true
	return aliases, nil
}

func (s *Store) invalidateAliasesCache() {
	s.aliasesMu.Lock()
	s.aliasesCache = nil
	s.aliasesCached = false
	s.aliasesMu.Unlock()
}

func (s *Store) SetDisplayAlias(ctx context.Context, kind, identity, alias string) error {
	kind = strings.TrimSpace(kind)
	identity = strings.TrimSpace(identity)
	alias = strings.TrimSpace(alias)
	if kind != AliasKindModel && kind != AliasKindMachine {
		return fmt.Errorf("unsupported alias kind %q", kind)
	}
	if identity == "" {
		return fmt.Errorf("alias identity is required")
	}
	if len([]rune(identity)) > 256 {
		return fmt.Errorf("alias identity is too long")
	}
	if len([]rune(alias)) > 48 {
		return fmt.Errorf("alias must be 48 characters or fewer")
	}
	if alias == "" {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM display_aliases WHERE kind = ? AND identity = ?`, kind, identity); err != nil {
			return err
		}
		s.invalidateAliasesCache()
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `
INSERT INTO display_aliases (kind, identity, alias, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(kind, identity) DO UPDATE SET alias = excluded.alias, updated_at = excluded.updated_at
`, kind, identity, alias, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	s.invalidateAliasesCache()
	return nil
}
