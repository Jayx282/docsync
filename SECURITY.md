# Security policy

## Reporting a vulnerability

Report security problems privately on GitHub: open the repository's **Security** tab and choose **Report a vulnerability** ([direct link](https://github.com/Jayx282/docsync/security/advisories/new)). Please don't open a public issue.

Include:

- what you found;
- how to reproduce it;
- the output of `docsync version`.

You should get a reply within a week. Once a fix is released, the advisory is published and credits you, unless you'd rather stay anonymous.

## Supported versions

Security fixes go into the latest release only. Please check that the problem still exists in it before reporting.

## How docsync handles secrets and access

These are the protections docsync relies on. A way around any of them is a vulnerability.

### Connection strings

- docsync reads the connection string from an environment variable, named by `source.dsn_env`.
- Studio never writes it to disk.
- The Windows service keeps it in the service's registry environment. If it contains a password, the service keeps it instead in a `dsn.secret` file that only Administrators, SYSTEM and the service account can read.
- Windows authentication avoids storing a password at all.

### Studio

- Studio listens on `127.0.0.1` by default and needs a random token. The token is saved in `<studio dir>/.token`, with mode 0600 on Linux and macOS.
- It rejects cross-site requests. On a loopback address it also rejects requests addressed to other host names, which blocks DNS rebinding.
- Its session cookie is `HttpOnly` and `SameSite=Strict`.
- Anyone signed in can read the database through previews, with the permissions of the connection string, and can deploy pipelines. Treat the token like a password.
- `--listen` with a non-loopback address prints a warning. Use it only on a network you trust.

### The dashboard

- `docsync run` serves the dashboard on `127.0.0.1:8088` by default.
- Its actions can pause, rebuild and resnapshot the pipeline.
- Before exposing it on another address:
  - turn on basic authentication (`dashboard.basic_auth_user` and `basic_auth_password_env`);
  - put it behind TLS.

### SQL

- Fragment queries run as written by whoever controls the config. docsync is not a sandbox for untrusted SQL.
- Studio formulas are different: they compile to T-SQL using only allow-listed functions, and table and column names are always quoted. A formula that makes Studio run other SQL is a vulnerability.
- The login needs only `SELECT` and `VIEW CHANGE TRACKING` on the tables a document reads. `doctor` and Studio's checks list the exact grants.
