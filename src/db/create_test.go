package db

import (
	"os"
	"testing"

	"github.com/efureev/db-migrator/src/config"
)

func Test_Create(t *testing.T) {
	InitConfigForTest()
	config.Get().Migrations.Path = `../.` + config.Get().Migrations.Path

	fileName := `Create Table`
	upFilePath, downFilePath := migrateCreate(fileName)

	if !isExistPath(upFilePath) {
		t.Fatalf("`%s` should be created", upFilePath)
	} else {
		os.Remove(upFilePath)
	}

	if !isExistPath(downFilePath) {
		t.Fatalf("`%s` should be created", downFilePath)
	} else {
		os.Remove(downFilePath)
	}
}

func isExistPath(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}
