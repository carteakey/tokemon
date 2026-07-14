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

func (s *Store) DisplayAliases(ctx context.Context) ([]DisplayAlias, error) {
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
	return aliases, nil
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
		_, err := s.db.ExecContext(ctx, `DELETE FROM display_aliases WHERE kind = ? AND identity = ?`, kind, identity)
		return err
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO display_aliases (kind, identity, alias, updated_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(kind, identity) DO UPDATE SET alias = excluded.alias, updated_at = excluded.updated_at
`, kind, identity, alias, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
