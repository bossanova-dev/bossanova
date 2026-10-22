package db

import (
	"database/sql"
	"errors"

	"github.com/recurser/bossd/internal/domain"
)

func notFound(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return domain.ErrNotFound
	}
	return err
}
