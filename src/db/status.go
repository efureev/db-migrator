package db

import (
	"database/sql"
	"fmt"
	"log"

	"github.com/efureev/db-migrator/src/config"
	_ "github.com/lib/pq"
)

// Status shows status of db-connection
func Status() {
	config.Check()
	size, dbStr := status()

	fmt.Printf("Database size: %s\n", size)
	fmt.Printf("conn: %s\n", dbStr)
}

func status() (string, string) {
	dbStr := connectionStr(config.Get().Database)
	db, err := sql.Open("postgres", dbStr)
	if err != nil {
		log.Fatalln(`[DB] Connection failed: ` + err.Error())
	}

	var size string

	s := `SELECT pg_size_pretty(pg_database_size($1)) as size`
	err = db.QueryRow(s, config.Get().Database.Name).Scan(&size)
	if err != nil {
		log.Fatalln(`[DB] Query failed: ` + err.Error())
	}

	return size, dbStr
}
