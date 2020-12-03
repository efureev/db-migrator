package db

import (
	"fmt"
	"log"
	"migrator/src/config"
	"os"
	"time"

	"github.com/iancoleman/strcase"
)

// MigrateCreate creates new migrate-file
func MigrateCreate(migrationNameFile *string) func() {
	return func() {
		log.Println(`creating migration...`)

		if migrationNameFile == nil {
			// migrationNameFile = ``
			log.Fatal(`не указано имя файла миграции`)
		}

		fn := strcase.ToSnake(*migrationNameFile)

		ts := time.Now().Unix()
		migrationPath := config.Get().Database.MigrationsPath

		createFile(fmt.Sprintf("%s/%d_%s.up.sql", migrationPath, ts, fn))
		createFile(fmt.Sprintf("%s/%d_%s.down.sql", migrationPath, ts, fn))

		log.Println(`migration has created!`)
	}
}

func createFile(filePath string) {
	// log.Println(`creating file: ` + filePath)

	w, err := os.Create(filePath)
	if err != nil {
		log.Panic(err)
	}
	defer w.Close()
	log.Println(`created file: ` + filePath)
}
