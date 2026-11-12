# Run docsync as a Windows service

Run a deployed pipeline on your Windows server, usually the one that runs SQL Server, as a Windows service. It then:
- starts with Windows;
- restarts after failures;
- keeps retrying while SQL Server is starting or unreachable;
- writes log files and Event Viewer entries.

With Windows authentication, no password is stored anywhere.

You need an administrator PowerShell window on the server.

## 1. Put docsync and the pipeline on the server

1. **Get `docsync.exe`.** Download `docsync_windows_amd64.exe` from the [latest release](https://github.com/Jayx282/docsync/releases/latest), or build it yourself with `make release` (it lands in `dist\`). Copy it to the server as, for example, `C:\docsync\docsync.exe`.
2. **Copy the pipeline's config.** Design and deploy the pipeline in Studio, then copy its `docsync.yaml` (in `studio\pipelines\<name>\`) to a folder of its own on the server, for example `C:\docsync\stone\docsync.yaml`.
   - The paths inside it are relative, so `state\` (the pipeline's own records) and `out\` (the documents) are created next to it.
   - You can also run Studio on the server itself and use the folder it deploys to.

## 2. Install the service

```powershell
C:\docsync\docsync.exe service install --config C:\docsync\stone\docsync.yaml --dsn "sqlserver://localhost?database=Stones&encrypt=disable"
```

This creates a service with these settings:

| Setting | Value |
|---|---|
| Name | `docsync-<document name>`, here `docsync-stone` (choose another with `--name`) |
| Account | `NT SERVICE\docsync-stone`, a virtual account Windows manages, with no password. It gets modify access to `C:\docsync\stone`. |
| Start | Automatic, shortly after Windows starts |
| On failure | Restart after 10 seconds, then 30 seconds, then every minute |
| Logs | `C:\docsync\stone\logs\docsync.log` (a new file every 10 MB; 5 old files are kept), and Event Viewer > Windows Logs > Application, source `docsync-stone` |

The command prints what it set up and the SQL for the next step, with your names filled in.

## 3. Let the service read the database

When SQL Server runs on the same machine, the service signs in with Windows authentication as its own account. Run this in SQL Server Management Studio:

```sql
CREATE LOGIN [NT SERVICE\docsync-stone] FROM WINDOWS;
USE [Stones];
CREATE USER [NT SERVICE\docsync-stone] FOR LOGIN [NT SERVICE\docsync-stone];
-- then the GRANT statements from Studio's Checks tab,
-- with [NT SERVICE\docsync-stone] in place of [docsync_login]
```

Studio's **Checks** tab also lists anything else the database needs, such as change tracking and indexes.

## 4. Start it and check it

```powershell
C:\docsync\docsync.exe service start --name docsync-stone
C:\docsync\docsync.exe service status --name docsync-stone
```

- **Dashboard:** http://127.0.0.1:8088 on the server. It shows lag, changes per table and errors, and has a document inspector. To open it from your own computer, use Remote Desktop, or an SSH tunnel if the server has OpenSSH.
- **Output:** `C:\docsync\stone\out`. Point your search service here; [consumer-protocol.md](consumer-protocol.md) says how to read it.
- **Services app:** the service also appears in Windows' **Services** app (`services.msc`) as "docsync (stone)".

## Day to day

| Task | How |
|---|---|
| Stop or start | `docsync service stop --name docsync-stone`, `docsync service start --name docsync-stone`, or the Services app |
| Change the design | Deploy in Studio, copy the new `docsync.yaml` over the old one, then stop and start the service. If the document definition changed, docsync rebuilds every document (Studio's deploy dialog tells you first). |
| Update docsync | Stop the service, replace `docsync.exe`, start it |
| See what happened | `logs\docsync.log` next to the config, or Event Viewer for starts, stops and retries |
| Remove it | `docsync service uninstall --name docsync-stone`. The folder, its documents and its state are kept. |

## When things go wrong

- **SQL Server is down, restarting or unreachable:** the service stays running. It logs "docsync stopped; starting it again", then waits 5 seconds, doubling up to 2 minutes, and tries again. Once SQL Server is back, it continues where it stopped.
- **docsync crashes:** Windows restarts it.
- **Something keeps failing:** the log and Event Viewer show the error on every retry. Fix the cause, for example a missing permission, and the next retry picks it up. Each retry also reads `docsync.yaml` again.

## Other ways to connect

- **SQL Server on another machine.** The virtual account reaches other machines as the server's computer account. On the remote SQL Server, create the login for that account:
  ```sql
  CREATE LOGIN [DOMAIN\SERVERNAME$] FROM WINDOWS;
  ```
  You can also run the service as a domain account:
  ```powershell
  $env:DOCSYNC_SERVICE_PASSWORD = Read-Host 'Password'   # typed here, not saved in the history
  C:\docsync\docsync.exe service install --config C:\docsync\stone\docsync.yaml --account DOMAIN\svc-docsync --dsn "sqlserver://dbserver?database=Stones"
  ```
- **A SQL login with a password.** When `--dsn` contains a password, docsync stores it in `dsn.secret` next to the config, not in the service's settings. Only SYSTEM, Administrators and the service account can read that file. To keep the password out of your PowerShell history, type it at a `Read-Host` prompt:
  ```powershell
  $dsn = Read-Host 'Connection string'   # typed here, not saved in the history
  C:\docsync\docsync.exe service install --config C:\docsync\stone\docsync.yaml --dsn $dsn
  ```
- **No `--dsn`.** The service then reads `DOCSYNC_DSN` (or the variable the config names) from the system environment variables.

## Linux

On Linux, run `docsync run` under systemd with `Restart=always`. Keep the connection string in an environment file only the service user can read:

```ini
# /etc/systemd/system/docsync-stone.service
[Unit]
Description=docsync (stone)
After=network-online.target
Wants=network-online.target

[Service]
User=docsync
ExecStart=/opt/docsync/docsync run --config /opt/docsync/stone/docsync.yaml
EnvironmentFile=/etc/docsync/stone.env
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
```

Put `DOCSYNC_DSN=...` in `/etc/docsync/stone.env` and give it mode 600. Then run `systemctl enable --now docsync-stone`. Logs go to `journalctl -u docsync-stone`.
