package db

import (
	"fmt"
	"log"
	"os"
	"time"

	"github.com/efureev/db-migrator/src/config"
	"github.com/iancoleman/strcase"
)

// MigrateCreate creates new migrate-file
func MigrateCreate(migrationNameFile *string) func() {
	return func() {
		migrateCreate(*migrationNameFile)
	}
}

func migrateCreate(migrationNameFile string) (string, string) {
	log.Println(`creating migration...`)

	if migrationNameFile == `` {
		log.Fatal(`не указано имя файла миграции`)
	}

	fn := strcase.ToSnake(migrationNameFile)

	ts := time.Now().Unix()
	migrationPath := config.Get().Migrations.Path

	upFilePath := createFile(fmt.Sprintf("%s/%d_%s.up.sql", migrationPath, ts, fn))
	downFilePath := createFile(fmt.Sprintf("%s/%d_%s.down.sql", migrationPath, ts, fn))

	log.Println(`migration has created!`)

	return upFilePath, downFilePath
}

func createFile(filePath string) string {
	w, err := os.Create(filePath)
	if err != nil {
		log.Panic(err)
	}
	defer w.Close()
	log.Println(`created file: ` + filePath)

	return filePath
}
