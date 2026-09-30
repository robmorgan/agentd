package session

// NameRules describes the names ValidName accepts. Session, workspace and
// remote host names all follow them.
const NameRules = "use 1-64 lowercase letters, numbers, and single hyphens"

// ValidName reports whether name is 1-64 lowercase letters, digits and
// single hyphens, neither starting nor ending with a hyphen.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 || name[0] == '-' || name[len(name)-1] == '-' {
		return false
	}
	lastHyphen := false
	for i := 0; i < len(name); i++ {
		switch c := name[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			lastHyphen = false
		case c == '-' && !lastHyphen:
			lastHyphen = true
		default:
			return false
		}
	}
	return true
}
