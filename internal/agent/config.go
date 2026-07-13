package agent

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultConfigPath returns the per-user agent configuration location shared
// by launchd, systemd, Homebrew, and container installers.
func DefaultConfigPath(home string) string {
	return filepath.Join(home, ".config", "tokemon", "agent.env")
}

// DefaultServerConfigPath returns the per-user server configuration location
// used by the macOS LaunchAgent and other managed server installs.
func DefaultServerConfigPath(home string) string {
	return filepath.Join(home, ".config", "tokemon", "server.env")
}

// ReadEnvFile reads a deliberately small dotenv-compatible file. It does not
// expand shell expressions or execute commands, so an agent config remains
// data rather than a script.
func ReadEnvFile(path string) (map[string]string, error) {
	values := make(map[string]string)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || !validEnvKey(key) {
			return nil, fmt.Errorf("%s line %d: expected KEY=VALUE", path, lineNumber)
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func validEnvKey(key string) bool {
	for index, character := range key {
		if (character >= 'A' && character <= 'Z') || (character >= '0' && character <= '9') || (character == '_' && index > 0) {
			continue
		}
		return false
	}
	return true
}
