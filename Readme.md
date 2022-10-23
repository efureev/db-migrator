# Migrator

Binary file to help you manage migrations into your DB.

# Features

- Create migration with custom name: `./migrator create -n file_name`
- Migrate up your migrations: `./migrator up`
- Migrate down your migrations: `./migrator down`
- Refresh all migrations: `./migrator fresh`
- Clean all you DB: `./migrator wipe`
- Version: `./migrator wipe`

## Build

```bash
./build.sh
```

## Examples

```
$ ./migrator create -n file_name
$ ./migrator create --name='create users table'
$ ./migrator up
$
$ MGTR_MIGRATIONS_PATH=./custom ./migrator up
$
$ MGTR_DATABASE_USER=fureev MGTR_DATABASE_NAME=kb-users MGTR_DATABASE_PORT=5432 go run main.go status 
```

### Docker

```shell
docker run -v /Volumes/Docker/data/kb/migrations:/migrations -v /Volumes/Docker/data/kb/migrator/config.test.toml:/app/config.toml --network host efureev/db-migrator up
```
