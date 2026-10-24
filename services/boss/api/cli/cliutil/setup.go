package cliutil

import (
	"fmt"
	"io"
	"strings"
)

func PrintSetupOutput(w io.Writer, text string) {
	_, _ = fmt.Fprint(w, text)
	if !strings.HasSuffix(text, "\n") {
		_, _ = fmt.Fprintln(w)
	}
}
