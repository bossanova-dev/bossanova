// Package agentsession validates agent session identifiers used in transcript paths.
package agentsession

// IsCanonicalUUID reports whether id is a lowercase hexadecimal UUID in
// 8-4-4-4-12 form. It validates the filename shape, without restricting UUID versions.
func IsCanonicalUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i := range len(id) {
		switch i {
		case 8, 13, 18, 23:
			if id[i] != '-' {
				return false
			}
		default:
			if !(id[i] >= '0' && id[i] <= '9' || id[i] >= 'a' && id[i] <= 'f') {
				return false
			}
		}
	}
	return true
}
