package gemini

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Verify the OS listener, not a tab title: another bridge may occupy the port.
// Unknown platforms fail closed until an equivalent listener check is provided.
func verifyProfile(port int, profile string) error {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return fmt.Errorf("Chrome profile verification unsupported on %s", runtime.GOOS)
	}
	pid, err := exec.Command("lsof", "-t", "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return fmt.Errorf("verify Chrome listener: %w", err)
	}
	id := strings.TrimSpace(string(pid))
	if _, err := strconv.Atoi(id); err != nil {
		return fmt.Errorf("ambiguous Chrome listener")
	}
	command, err := exec.Command("ps", "-p", id, "-o", "command=").Output()
	if err != nil {
		return err
	}
	return validateProfileCommand(string(command), port, profile)
}

func validateProfileCommand(command string, port int, profile string) error {
	profile, err := filepath.Abs(profile)
	if err != nil {
		return err
	}
	command = strings.TrimSpace(command)
	expected := "--user-data-dir=" + profile
	// Chrome's ps command line is unquoted, so use the argument boundary rather
	// than whitespace splitting (profile paths can contain spaces).
	if !(strings.Contains(command, " "+expected+" ") || strings.HasSuffix(command, " "+expected)) ||
		!strings.Contains(command+" ", " --remote-debugging-port="+strconv.Itoa(port)+" ") {
		return fmt.Errorf("Chrome listener does not match configured profile and port")
	}
	return nil
}
