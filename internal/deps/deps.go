package deps

import (
	"fmt"
	"os/exec"
	"strings"
)

// Check verifies if a command is available in PATH
func Check(command string) bool {
	_, err := exec.LookPath(command)
	return err == nil
}

// CheckRequired verifies only the dependencies needed for a specific operation
func CheckRequired(deps ...string) error {
	var missing []string
	for _, dep := range deps {
		if !Check(dep) {
			missing = append(missing, dep)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing dependencies: %s", strings.Join(missing, ", "))
	}

	return nil
}
