# Security

## Supported versions

`v2.x`. The 1.x line is not maintained.

## Reporting

Use GitHub's private advisory form on this repository. Please do not open a
public issue for something exploitable.

## In scope

- **The password never reaches the output.** Not the log, not an error message,
  not `migrator config`. It is removed by rebuilding a parsed DSN, never by a
  regular expression over the original text, and a DSN that does not parse is
  not printed at all — the usual reason one fails to parse is a password holding
  a character that needed escaping. `pgconn` embeds the whole connection
  configuration in the text of a connection error, so every error from it is
  redacted before it is shown. There are tests for this.
- **Identifier injection through configuration.** The schema and table names are
  interpolated into DDL, because a bind parameter cannot stand in for an
  identifier. They are checked against a strict pattern at construction and
  quoted with `pgx.Identifier.Sanitize` at the point of use.
- **The guards on destructive operations.** A way to make `wipe` or `down` run
  where the documented guards say they must not is a security issue, not a bug.

## Out of scope

- **The SQL inside migration files.** That is the operator's SQL and the tool
  runs it deliberately. A migration that drops a table drops the table.
- **Protecting the `.env` file that holds the password.** Its permissions and
  its presence in a repository are the caller's business.
- **Denial of service through the advisory lock.** Anyone who can connect to the
  database can take it. So can anyone who can run `psql`.
