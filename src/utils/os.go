package utils

import (
	"os"
)

func CreateDir(path string) error {
	return os.MkdirAll(path, 0755)
}

// Проверка на существование файла или директории
func IsExistPath(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

func CreateNotExistDir(path string) error {
	if !IsExistPath(path) {
		return CreateDir(path)
	}

	return nil
}
