package main

import (
	"io"
	"time"

	"github.com/recurser/boss/internal/tuidriver"
)

type castWriter = tuidriver.CastWriter

func newCastWriter(w io.Writer, cols, rows int, now func() time.Time) (*castWriter, error) {
	return tuidriver.NewCastWriter(w, cols, rows, now)
}
