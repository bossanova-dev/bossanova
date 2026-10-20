package domain

import "errors"

// ErrNotFound indicates that the requested domain entity does not exist.
var ErrNotFound = errors.New("not found")
