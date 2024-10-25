# Migrator

Binary file to help you manage migrations into your DB.

# Features

- Create migration with custom name: `./migrate create -n file_name`
- Migrate up your migrations: `./migrate up`
- Migrate down your migrations: `./migrate down`
- Refresh all migrations: `./migrate fresh`
- Clean all you DB: `./migrate wipe`
- Migrations Version: `./migrate db:version`
- App Version: `./migrate version`

## Build

```bash
./build.sh
```

## Examples

```shell
$ ./migrate --help
$ ./migrate version
$ ./migrate create -n file_name
$ ./migrate create --name='create users table'
$ ./migrate up
$
$ MIGRATION_DIR=./custom ./migrate up 
```

```shell
DB_HOST=pg DB_PORT=5434 DB_USER=fureev DB_NAME=test MIGRATION_DIR=./migrations migrate 
```

### Docker

Without config file

```shell
docker run -v /Volumes/Docker/project/migrations:/migrations \
  -e DB_USER=fureev \
  -e DB_NAME=testdb \
  --network host \
  ghcr.io/efureev/db-migrator create -n 'create user table'
```

```shell
docker run -v /Volumes/Docker/project/migrations:/migrations \
  -e DB_USER=fureev \
  -e DB_PASS=pass \
  -e DB_PORT=5435 \
  -e DB_NAME=testdb \
  --network host \
  ghcr.io/efureev/db-migrator status
```

With config file

```shell
docker run -v /Volumes/Docker/project/migrations:/migrations \
  -v /Volumes/Docker/data/kb/migrator/config.test.yaml:/app/config.yaml \
  --network host \
  ghcr.io/efureev/db-migrator status
```

or define config file:

```shell
docker run --network host \
  -v /Volumes/Docker/project/migrations:/migrations \
  -v /Volumes/Docker/data/kb/migrator/config.test.yaml:/app/config.yaml \
  ghcr.io/efureev/db-migrator -f /app/config.yaml status
```

Where config file:

```yaml
[migrations]
path = "/migrations"

[database]
user = "fureev"
password = ""
host = "localhost"
port = 5432
name = "testing"
```

More examples:

```shell
docker run -v /Volumes/Docker/data/kb/migrations:/migrations -e MGT_DATABASE_USER:fureev --network host ghcr.io/efureev/db-migrator status
docker run -v /Volumes/Docker/data/kb/migrations:/migrations -v /Volumes/Docker/data/kb/migrator/config.test.yaml:/app/config.yaml --network host ghcr.io/efureev/db-migrator up
```
