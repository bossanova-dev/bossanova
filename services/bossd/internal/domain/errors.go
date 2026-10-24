package domain

import "errors"

// ErrNotFound indicates that the requested domain entity does not exist.
var ErrNotFound = errors.New("not found")

var ErrCredentialNotFound = errors.New("account credential not found")
