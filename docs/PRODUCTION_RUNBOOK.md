# Production runbook

A checklist for bringing up and running the **production** environment of the Railway project. It assumes the README is at hand for details ([Operations](../README.md#operations), [Settings](../README.md#settings)) and doesn't repeat it.

- **Placeholders:** `<production-domain>` is the service's Railway domain (`RAILWAY_PUBLIC_DOMAIN`), and `$PROD_DB` is the Production database's public URL (step 2).
- **Secrets:** never paste a secret into a command line, a file in this repository or a chat; use `read -s`.

**How the server knows it is Production:** `RAILWAY_ENVIRONMENT_NAME` (set by Railway) equals `production`. Then every startup check below is enforced, and an unsafe configuration stops the deploy. In any other environment, the same problems are only logged as `WARN`.

## 1. Pre-flight: variables

Set these on the API service **in the production environment** before the first deploy:

| Variable | Status | Value |
|---|---|---|
| `DATABASE_URL` | required | A reference to the Production Postgres service's `DATABASE_URL` (the private URL). **Don't add a `timezone` parameter** (step 2). |
| `JWT_SECRET` | required | `openssl rand -base64 48`, **different from Staging**. The server refuses one shorter than 32 characters, with fewer than 12 distinct characters, or containing a placeholder word (`secret`, `changeme`, `password`, …). The code can't compare it with Staging's, so make sure yourself. |
| `FIREBASE_CREDENTIALS_JSON` | required | The JSON of a **separate service-account key for Production**, not Staging's. It must be a `service_account` with `project_id`, `client_email` and a PEM `private_key`. Alternative: `FIREBASE_CREDENTIALS_PATH` pointing at a file in the image. |
| `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` | required | `30` (graceful shutdown; see the README) |
| `TRUSTED_PROXY_CIDRS` | after first boot | Chosen in step 5. Until then the server logs an `ERROR` at every boot. |
| `SHUTDOWN_TIMEOUT`, `ABSENCE_CRON_SCHEDULE`, `DB_SSL_MODE`, `DB_MAX_OPEN_CONNS`, `DB_MAX_IDLE_CONNS`, `DB_CONN_MAX_LIFETIME` | optional | Defaults are in the README and `internal/config/config.go`. |
| `PORT` | Railway sets it | — |
| `FAKE_TODAY` | **must not exist** | Tests only; the server refuses to start on Railway with it. |

## 2. Database

1. **Create** a PostgreSQL service in the **production** environment.
2. **Enable Public Access** on it only for the migration and backup steps. Railway then "creates a TCP Proxy and populates a `DATABASE_PUBLIC_URL`" ([docs](https://docs.railway.com/guides/postgresql)), and the proxy's egress is billed. In a local shell:
   ```bash
   read -r -s -p "Production DATABASE_PUBLIC_URL: " PROD_DB; echo; export PROD_DB
   ```
3. **Time zone (audit W2):** no database step is needed. The server pins every connection to `timezone=Asia/Baghdad` (`config.BuildDBURL`) and never relies on the database's default time zone (UTC on Railway).
   - **Keep `DATABASE_URL` free of a `timezone` parameter:** one in the URL is kept and overrides the pin.
   - **Manual SQL** must convert explicitly, for example `(now() AT TIME ZONE 'Asia/Baghdad')::date`.
4. **Build the schema from empty with every migration** (golang-migrate CLI). The repository has **33** (`000001` to `000033`):
   ```bash
   migrate -path db/migrations -database "$PROD_DB" up
   migrate -path db/migrations -database "$PROD_DB" version     # expect 33
   ```

## 3. Rotate the admin password before the first deploy

Migration `000008` seeds `admin` with a public password. **The server refuses to start in Production while any admin account still accepts it**, so do this before step 4.

1. **Generate a password:** `openssl rand -base64 32`.
2. **Follow [db/scripts/README.md](../db/scripts/README.md):**
   - copy `db/scripts/rotate_admin_password.sql` to the git-ignored `db/scripts/rotate_admin_password.with_password.sql`;
   - replace `NEW_PASSWORD_HERE` in the copy;
   - then:
     ```bash
     psql "$PROD_DB" -v ON_ERROR_STOP=1 -f db/scripts/rotate_admin_password.with_password.sql   # ends with UPDATE 1 and COMMIT
     rm db/scripts/rotate_admin_password.with_password.sql
     ```
3. **Store the username and password in the password manager.** Nothing else keeps them.

## 4. Deploy and check the first boot

Deploy the API service in the production environment.

**The first boot's log must show, in order:**
- `Production startup checks passed`
- `Server started`

**If the deploy fails instead,** the log lists every problem at once:
- one `Startup check failed` line each, with a `problem` naming the variable and the rule (never a value);
- then `Refusing to start in production: startup checks failed; see docs/PRODUCTION_RUNBOOK.md`.

Fix all of them and redeploy.

**Health check:**
```bash
curl -s https://<production-domain>/health     # HTTP 200, {"message":"Server is healthy and running!","status":"success"}
```

## 5. TRUSTED_PROXY_CIDRS

Until it is set, every request appears to come from Railway's proxy, so the admin-login rate limits are shared by every client. In Production the server logs this as an `ERROR` at every boot: `TRUSTED_PROXY_CIDRS is not set: … (docs/PRODUCTION_RUNBOOK.md, step 5)`.

1. **Make a few requests** from a known network (for example `curl https://<production-domain>/health` and an admin login).
2. **Read the log lines `Observed client address`.** The first 20 requests after each start log `remote_addr`, `x_real_ip`, `x_forwarded_for` and `client_ip`.
3. **Choose the value:** the address range that `remote_addr` comes from, which is Railway's proxy. Choose it only if `x_real_ip` (or the rightmost `x_forwarded_for` entry) shows **your** public address. Comma-separated CIDRs or addresses are accepted.
4. **Set** `TRUSTED_PROXY_CIDRS` and redeploy.
5. **Confirm:**
   - the `ERROR` is gone;
   - the log shows `Trusting client-IP headers from proxies`;
   - the new `Observed client address` lines show your address as `client_ip`.

## 6. Settings

The migrations seed a placeholder `whatsapp_number` (`+9640000000000`) and a `school_name`. Set both through the admin API (`PUT /api/admin/settings`, one key per request):

```bash
BASE_URL=https://<production-domain>
read -r -p "Admin username: " ADMIN_USERNAME
read -r -s -p "Admin password: " ADMIN_PASSWORD; echo
ADMIN=$(jq -nc --arg u "$ADMIN_USERNAME" --arg p "$ADMIN_PASSWORD" '{username:$u,password:$p}' \
  | curl -s -X POST "$BASE_URL/api/admin/login" -H 'Content-Type: application/json' -d @- | jq -r .data.token)
unset ADMIN_PASSWORD
curl -s -X PUT "$BASE_URL/api/admin/settings" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"key":"whatsapp_number","value":"<school WhatsApp number>"}'
curl -s -X PUT "$BASE_URL/api/admin/settings" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
  -d '{"key":"school_name","value":"<school name>"}'
curl -s "$BASE_URL/api/mobile/settings"     # public: shows both
```

## 7. Devices

1. **Register each device by serial number:**
   ```bash
   curl -s -X POST "$BASE_URL/api/admin/devices" -H "Authorization: Bearer $ADMIN" -H 'Content-Type: application/json' \
     -d '{"serial_number":"<device SN>","location_name":"<gate>","is_active":true}'
   ```
2. **Device user IDs** equal the students' `rfid_tag`:
   - 1 to 14 digits, **never a leading zero**;
   - by school convention, **1–99 for staff and 100+ for students**.

   An ID that matches no student is skipped (WARN `ADMSHandler: punches for unknown device PINs skipped`).
3. **Clear the device's stored attendance records first.** Use its data-management menu (the menu path depends on the model).
   - **Why:** a stored backlog is pushed when the device connects. Old punches are stored as attendance, and today the server also sends their check-in and check-out notifications to parents, days late.
4. **Point the device at Production:** in its cloud (ADMS) server settings, set the server address to `<production-domain>`. Keep the port and protocol settings the Staging device uses; only the address changes.
5. **Confirm:** `GET /api/admin/devices` shows a recent `last_sync` for the device, and a test punch by a staff ID appears in the logs.

## 8. Backups

**Manual dump** (before go-live, before every release with a migration, and on a regular schedule):

```bash
psql "$PROD_DB" -XAtc 'SHOW server_version'   # the server's major version, e.g. 16.x
pg_dump --version                               # must have the same major version; install that client if not
pg_dump --no-owner --no-acl -Fc -d "$PROD_DB" -f "futurekids-prod-$(date +%F).dump"
```

- The dump contains children's and parents' personal data.
- Keep it encrypted, outside this repository, and delete old copies on purpose.

**Restore test** (at least once before go-live), into a local throwaway database:

```bash
createdb -h localhost fk_restore_check
pg_restore --no-owner --no-acl -d postgres://localhost:5432/fk_restore_check "futurekids-prod-<date>.dump"
psql postgres://localhost:5432/fk_restore_check -XAtc "SELECT version, dirty FROM schema_migrations; SELECT count(*) FROM students; SELECT count(*) FROM attendance_logs;"
dropdb -h localhost fk_restore_check
```

### Automated backups: decision pending

Nothing is automated yet, and this runbook builds no automation.

**What Railway documents** ([backups](https://docs.railway.com/reference/backups)):
- volume backups "can also be scheduled to run on a Daily / Weekly / Monthly schedule", kept 6, 27 and 89 days respectively;
- "Backups can only be restored into the same project + environment";
- "Wiping a volume deletes all backups".

**To decide:**
- whether to enable them for the Production Postgres volume, and at what cost and plan;
- whether off-Railway manual dumps (above) stay the copy that survives losing the environment.

## 9. Monitoring

**Railway logs.** In the first week, search the production service's logs for these messages (exact text from the code):

| Message | Meaning |
|---|---|
| `Startup check failed`, `Refusing to start in production` | unsafe configuration; the deploy failed |
| `TRUSTED_PROXY_CIDRS is not set` | step 5 not done |
| `Railway will SIGKILL this process before graceful shutdown can finish` | `RAILWAY_DEPLOYMENT_DRAINING_SECONDS` is missing |
| `ADMSHandler: punches for unknown device PINs skipped` | a device ID with no student (`rfid_tag` mismatch) |
| `ADMSHandler: unregistered device attempted ADMS push`, `ADMSHandler: disabled device attempted ADMS push`, `DeviceAuthMiddleware: unregistered device rejected` | a device not registered in step 7, or disabled |
| `ADMSHandler: batch rolled back — replying 503 so the device resends it`, `ADMSHandler: punch rejected by the database — skipped` | database trouble while storing punches |
| `parseATTLOG:` (any) | malformed device data |
| `AdminLoginHandler: login rate-limited`, `MobileLoginHandler: login rate-limited`, `AccountDeleteHandler: login rate-limited` | repeated wrong passwords or PINs |
| `Failed to send FCM message`, `Broadcast push: send failed`, `FCM token is no longer registered; deleted it` | push problems (the last is routine in small numbers) |
| `ProcessDailyAbsences:` with `failed` | the noon absence job failed |
| `Activity log: insert failed` | the admin activity log is missing entries |
| `school_name setting is missing` | step 6 not done |
| `Shutdown incomplete` | a deploy cut off work in progress |
| `background: recovered panic` | a bug; report it |

**Uptime.** Point any external uptime monitor (any provider, no lock-in) at `https://<production-domain>/health`, every 1–5 minutes. Alert on anything other than HTTP 200.

## 10. Rollback

- **Code:** in Railway, a previous deployment's menu has **Rollback** ("Both the Docker image and custom variables are restored", within the plan's retention) and **Redeploy** ([docs](https://docs.railway.com/guides/deployment-actions)).
  - Every migration is additive and the previous code runs on the newer schema; each release's notes in the README say so.
  - So rolling back the code alone is the normal path.
- **Migrations:** only when unavoidable, one step at a time, after reading the down-migration's header.
  - `-- ⚠ APP-COMPATIBILITY WARNING` names the code that breaks and the data that is lost (audit W14).
  - See [db/migrations/README.md](../db/migrations/README.md).
  - Take a dump first, and roll the code back **before** the migration.
- **Data:** restore a dump (step 8) into a new database and switch `DATABASE_URL` to it. Never restore over the live database without a fresh dump of it first.

## 11. Release process

**The problem:** today both environments build from `main`. Once Production is live, every push to `main` would deploy to Production at the same moment as to Staging, before anyone has checked it on Staging, and before a migration has been applied to Production.

**What Railway offers** ([GitHub autodeploys](https://docs.railway.com/guides/github-autodeploys)):
- "To update the branch that triggers automatic deployments, go to your Service Settings and choose the appropriate trigger branch." Railway lists separate environments that "auto-deploy when changes are made to different branches" as a use case ([environments](https://docs.railway.com/guides/environments)).
- Autodeploy can be turned off: "Click **Disable** to stop deploying automatically on new commits". "To manually trigger a deployment from the latest commit, open the Command Palette (`CMD + K`) and select **Deploy Latest Commit**."
- **Wait for CI:** "new deployments sit in a `WAITING` state until every GitHub Actions check suite on the commit has finished."

**The options:**

| Option | How | Trade-offs |
|---|---|---|
| **A. Production tracks a `production` branch** | Production's trigger branch is `production`; Staging keeps `main`. A release fast-forwards `production` to a `main` commit already verified on Staging. | **For:** Production gets exactly the commit tested on Staging; a release is a visible git event (`git log production`); rollback is the Railway rollback or resetting the branch. **Against:** one more branch to keep in sync; never commit to `production` directly. |
| **B. Autodeploy off in Production, deploy by hand** | Disable autodeploy for Production; deploy with **Deploy Latest Commit**. | **For:** nothing reaches Production unless someone deploys it. **Against:** it deploys the latest commit of `main`, which may already be newer than what was tested on Staging; the release isn't recorded in git. |
| **C. Wait for CI** | Enable **Wait for CI**. | **For:** a commit that fails CI never deploys. **Against:** the repository has no GitHub Actions yet, so it gates nothing today. It also doesn't separate Production from Staging: a passing commit still deploys to both at once. It's a complement to A or B, not a substitute. |

**Recommendation: A**, and C later once CI exists.

**A release then is:**
1. merge to `main`;
2. Staging deploys; verify it (the README's per-release check);
3. take a Production dump (step 8);
4. if the release has a migration, run it on Production first, following that release's README notes (migrations are additive, so the old code keeps running on the new schema);
5. `git push origin main:production` (a fast-forward);
6. check the deploy log for `Production startup checks passed`.

**Set up once:**
- create the `production` branch at the commit Production should run;
- set Production's trigger branch to it.

Nothing in this runbook configures Railway for you.

## 12. Go-live day checklist

- [ ] Section 1: all variables set in the **production** environment; `FAKE_TODAY` absent; `JWT_SECRET` and the Firebase key are Production's own.
- [ ] Section 2: `migrate … version` prints **33** (or the current count); Public Access disabled again if not needed.
- [ ] Section 3: admin password rotated and stored in the password manager.
- [ ] Section 4: **After deploy, confirm the log line `Production startup checks passed` appears. If it does not, the checks are not active.** That is the case if the environment is no longer named `production`: every check then only warns. `/health` answers 200.
- [ ] Section 5: `TRUSTED_PROXY_CIDRS` set; no `TRUSTED_PROXY_CIDRS is not set` ERROR after the redeploy.
- [ ] Section 6: `GET /api/mobile/settings` shows the real `whatsapp_number` and `school_name`.
- [ ] Section 7: every device registered, its stored records cleared, pointed at Production, `last_sync` recent; one test punch seen.
- [ ] Section 8: a fresh dump taken and a restore test passed.
- [ ] Section 9: uptime monitor on `/health` running; the log searches bookmarked.
- [ ] Section 11: Production's trigger branch decided and set.

## 13. Never on Production

- **`cmd/seed-staging`:** it fills a database with fake students, parents and notifications through the API. It refuses every `BASE_URL` except `https://futurekids-staging.up.railway.app` (and a local URL only with `--allow-local`), so it can't reach Production unless someone edits that code. Don't.
- **`FAKE_TODAY`:** pins "today" for tests; the server refuses to start on Railway with it.
- **The e2e script and the Go tests against a Production database:**
  - they need `TEST_DATABASE_URL` and refuse any host that isn't `localhost`, `127.0.0.1` or `::1`, and any host containing `railway` (audit W11, `internal/testdb` and `tests/e2e_test.sh`);
  - they only create and drop their own `fk_test_*` databases, and never read `DATABASE_URL`;
  - **never point `TEST_DATABASE_URL` at a local tunnel or port-forward to Production:** it would pass the host check.
- **`cmd/migrate-pins`:** an obsolete one-off from before PINs were hashed; it writes to whatever `DATABASE_URL` it's given.
- **The default admin:** the server refuses to start in Production while any admin account accepts the password from migration `000008`. Rotate it (section 3) on every database that ever serves Production, including a restored one.
- **Down-migrations without reading their header and taking a dump** (section 10).
