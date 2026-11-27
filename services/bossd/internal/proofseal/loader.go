package proofseal

import (
	"bytes"
	_ "embed"
)

// Object names a sealed page stores besides its sealed content files.
const (
	// LoaderName is the plaintext loader; it is put last, so its presence
	// marks a complete page.
	LoaderName = "index.html"
	// ShimName is the plaintext script the loader runs to open the page.
	ShimName = "proof-shim.js"
	// PageName is the sealed rendered page the shim opens.
	PageName = "page.html.enc"
	// SealedSuffix ends the name of every other sealed content file.
	SealedSuffix = ".enc"
)

var (
	//go:embed loader/index.html
	loaderHTML []byte
	//go:embed loader/proof-shim.js
	shimJS []byte
)

// Loader returns the plaintext loader index.html.
func Loader() []byte { return bytes.Clone(loaderHTML) }

// Shim returns the plaintext proof-shim.js.
func Shim() []byte { return bytes.Clone(shimJS) }

// SealedName is the object name a content file is stored under once sealed:
// the rendered page index.html becomes PageName, and every other file gains
// SealedSuffix, so no sealed object is served as its plaintext type.
func SealedName(name string) string {
	if name == LoaderName {
		return PageName
	}
	return name + SealedSuffix
}
