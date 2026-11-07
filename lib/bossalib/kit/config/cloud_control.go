package config

import "strings"

// CloudControlMode is the local operator's authority granted to cloud commands.
type CloudControlMode string

const (
	// CloudControlReadOnly allows observation and locally verified housekeeping.
	CloudControlReadOnly CloudControlMode = "read-only"
	// CloudControlPrivate allows metadata observation only.
	CloudControlPrivate CloudControlMode = "private"
	// CloudControlFull also allows commands that change local state.
	CloudControlFull CloudControlMode = "full"
)

// ResolveCloudControl fails closed. Only exact lowercase "full", after trimming
// whitespace, enables control; unknown raw values survive settings round trips.
func ResolveCloudControl(raw string) CloudControlMode {
	switch CloudControlMode(strings.TrimSpace(raw)) {
	case CloudControlFull:
		return CloudControlFull
	case CloudControlPrivate:
		return CloudControlPrivate
	case CloudControlReadOnly:
		return CloudControlReadOnly
	}
	return CloudControlReadOnly
}
