package gemini

import "testing"

func TestValidateProfileCommand(t *testing.T) {
	for _, tc := range []struct {
		command string
		ok      bool
	}{
		{"Chrome --remote-debugging-port=9223 --user-data-dir=/tmp/my profile about:blank", true},
		{"Chrome --remote-debugging-port=9222 --user-data-dir=/tmp/my profile about:blank", false},
		{"Chrome --remote-debugging-port=9223 --user-data-dir=/tmp/my profile-other about:blank", false},
	} {
		if got := validateProfileCommand(tc.command, 9223, "/tmp/my profile") == nil; got != tc.ok {
			t.Errorf("command %q: got %v", tc.command, got)
		}
	}
}
