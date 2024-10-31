# Migrator

Docker/Binary-file to help you manage migrations into your DB.

## Features

- Create migration with custom name: `./migrate create -n 'create user table'`
- Migrate up your migrations: `./migrate up`
- Migrate down your migrations: `./migrate down`
- Refresh all migrations: `./migrate fresh`
- Clean all tables in your DB: `./migrate wipe`
- Migrations Version: `./migrate db:version`
- App Version: `./migrate version`

## Image

| Registry                                            | Image                     |
|-----------------------------------------------------|---------------------------|
| [GitHub Container Registry][link_github_containers] | `ghcr.io/efureev/migrate` |
| [Docker Hub][link_docker_tags]                      | `feugene/migrate`         |

> Images, based on the `alpine` image has a postfix `-alpine` in the tag name, e.g.: `feugene/migrate:1.4.8-alpine`.

Following platforms for this image are available:

```shell
docker run --rm mplatform/mquery feugene/migrate:latest
```

**Supported platforms**:

- linux/amd64
- linux/386
- linux/arm64
- linux/arm/v7
- linux/arm/v8

## Use

```shell
docker run --rm --network host \
  -v ./migrations:/migrations \
  -v ./config.yaml:/tmp/config.yaml \
  feugene/migrate:latest -f /tmp/config.yaml status
```

You can define config file (yaml) `-f /tmp/config.yaml`:

```yaml
migrations:
  dir: "./migrations"

database:
  host: "localhost"
  port: 5432
  name: "dbname"
  user: "user"
  pass: ""
```

Or use OS Envs:

```shell
docker run --rm --network host \
  -v ./migrations:/migrations \
  -e DB_USER=fureev \
  -e DB_NAME=testdb \
  feugene/migrate:latest create -n 'create user table'
```

List of all Env vars:

- DB_HOST=localhost
- DB_PORT=5432
- DB_USER=
- DB_PASS=
- DB_NAME=
- MIGRATION_DIR=/migrations


>Env-variables have higher priority over config-file.

## For Developers

### Build

```shell
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

[link_docker_tags]:https://github.com/efureev/db-migrator/tags
[link_github_containers]:https://github.com/efureev/db-migrator/pkgs/container/migrate