package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/recurser/bossd/internal/domain"
)

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		// Preserve legacy callers while stores adopt the domain sentinel.
		return fmt.Errorf("%w: %w", domain.ErrNotFound, err)
	}
	return err
}
