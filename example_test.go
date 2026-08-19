package migrator_test

import (
	"fmt"
	"os"
	"testing/fstest"

	migrator "github.com/efureev/db-migrator/v2"
)

// Reading a set of migrations needs no database. Everything a source can be
// wrong about — a malformed name, two files claiming one version, an unknown
// directive, an unterminated quote — is found here, before anything connects.
func ExampleLoad() {
	// In a real project this is os.DirFS("./migrations"), or an embed.FS:
	//
	//	//go:embed migrations/*.sql
	//	var embedded embed.FS
	//	sub, _ := fs.Sub(embedded, "migrations")
	src := fstest.MapFS{
		"20240117090000_create_users.up.sql": &fstest.MapFile{
			Data: []byte("CREATE TABLE users (id bigint PRIMARY KEY);\n"),
		},
		"20240117090000_create_users.down.sql": &fstest.MapFile{
			Data: []byte("DROP TABLE users;\n"),
		},
		"20240118120000_add_users_email_index.up.sql": &fstest.MapFile{
			Data: []byte("-- migrator:no-transaction\n" +
				"CREATE INDEX CONCURRENTLY users_email_idx ON users (email);\n"),
		},
	}

	set, err := migrator.Load(src)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)

		return
	}

	for m := range set.All() {
		fmt.Printf("%s  down=%t  no-transaction=%t\n",
			m, m.HasDown(), m.Directives.NoTransaction)
	}

	// Output:
	// 20240117090000_create_users  down=true  no-transaction=false
	// 20240118120000_add_users_email_index  down=false  no-transaction=true
}

// A source with problems reports all of them at once, each naming its file.
// Fixing a directory one error per run turns a minute of work into a quarter of
// an hour, so Load never stops at the first.
func ExampleLoad_problems() {
	_, err := migrator.Load(fstest.MapFS{
		"CreateUsers.up.sql":   &fstest.MapFile{Data: []byte("SELECT 1;")},
		"0001_empty.up.sql":    &fstest.MapFile{Data: []byte("-- nothing here\n")},
		"0002_typo.up.sql":     &fstest.MapFile{Data: []byte("-- migrator:no-transacton\nSELECT 1;")},
		"0003_orphan.down.sql": &fstest.MapFile{Data: []byte("DROP TABLE c;")},
	})

	fmt.Println(err)

	// Unordered output:
	// CreateUsers.up.sql: migrator: filename does not match <version>_<name>.(up|down).sql
	// 0001_empty.up.sql: migrator: up file is empty
	// 0002_typo.up.sql: migrator: unknown migrator directive: "no-transacton"
	// 0003_orphan.down.sql: migrator: down file has no matching up file
}

// The checksum covers the file in a canonical form, so that a Windows checkout
// with core.autocrlf=true does not look like somebody having edited a released
// migration — and a real edit still does.
func ExampleChecksum() {
	unix := []byte("CREATE TABLE users (id int);\n")
	windows := []byte("CREATE TABLE users (id int);\r\n")
	edited := []byte("CREATE TABLE users (id bigint);\n")

	fmt.Println(migrator.Checksum(unix) == migrator.Checksum(windows))
	fmt.Println(migrator.Checksum(unix) == migrator.Checksum(edited))

	// Output:
	// true
	// false
}
