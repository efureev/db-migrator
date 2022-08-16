package db

import (
	"database/sql"
	"fmt"

	_ "github.com/lib/pq"
	"migrator/src/config"
)

// Status shows status of db-connection
func Status() {

	dbStr := connectionStr(config.Get().Database)
	db, err := sql.Open("postgres", dbStr)
	if err != nil {
		panic(err)
	}

	var size string

	s := `SELECT pg_size_pretty(pg_database_size($1)) as size`
	err = db.QueryRow(s, config.Get().Database.Name).Scan(&size)
	if err != nil {
		fmt.Printf("call to database failed: %s", err)
		return
	}

	fmt.Printf("Database size: %s\n", size)
	fmt.Printf("conn: %s\n", dbStr)
}
