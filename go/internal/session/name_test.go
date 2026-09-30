package session

import (
	"strings"
	"testing"
)

func TestValidSessionName(t *testing.T) {
	for name, want := range map[string]bool{
		"a": true, "a-b": true, "abc-123": true, strings.Repeat("a", 64): true,
		"": false, "-a": false, "a-": false, "a--b": false, "A": false, "a_b": false,
		"é": false, "a/b": false, "..": false, strings.Repeat("a", 65): false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("%q: got %v, want %v", name, got, want)
		}
	}
}
