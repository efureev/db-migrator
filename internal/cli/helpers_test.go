package cli

import (
	"os"
	"path/filepath"
)

// writeFiles creates a set of files under root, making directories as needed.
func writeFiles(root string, files map[string]string) error {
	for name, body := range files {
		path := filepath.Join(root, name)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}

		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			return err
		}
	}

	return nil
}
