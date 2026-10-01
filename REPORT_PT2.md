I've finished the OpenAPI audit: the spec now matches what the server actually does. A new contract test runs the real server, hits all 30 operations, and triggers every documented status code, including the 500s and 503s. Build, vet, gofmt, the full `-race` suite and the isolated e2e (51 assertions) all pass. No test databases or server processes were left behind; your server on 8080 was not touched.

**Written for:** the external Flutter developer building the parent app ([docs/FLUTTER_HANDOFF.md](docs/FLUTTER_HANDOFF.md)).

### What I changed
- **[future_kids_api.yaml](future_kids_api.yaml)** is rewritten to match the server:
  - every operation has an operationId, summary, tags, security and examples;
  - request and response schemas are separate where they differ;
  - `required` and `nullable` match what the server sends;
  - times and dates have exact format patterns;
  - examples use only fake data.
- **Hardware routes:** I kept them in the same file, tagged `Hardware (internal)` and marked `x-internal`. RULES §4 makes this one file the contract for every route, and the drift test needs every route in one place. `npx @redocly/cli bundle flutter` produces a copy without them.
- **[redocly.yaml](redocly.yaml)** (new) holds the lint config and the Flutter-only bundle.
- **[tests/contract/](tests/contract/)** (new) holds the contract test.
  - It fails if a route in `cmd/api/main.go` and the spec disagree, or if any response differs from the spec in status, content type, headers, JSON shape, types, nullability or required fields.
  - It covers every negative case you listed.
  - go.mod has no YAML library, so the test uses a small strict YAML parser. I confirmed it reads the spec exactly as Redocly does.
  - I broke copies of the spec in 15 different ways, and each one was caught by the check aimed at it.
  - Run against the old spec, the test fails 3 of its 4 checks. 66 operation/status pairs were undocumented (431 ×30, 403 ×20, 413 ×9, 500 ×6, 400 ×1).
- **README** has a short "API contract" section.

### Mismatches found

| Where | Old spec said | Code does | Who's right | Change |
|---|---|---|---|---|
| `servers` | Production and localhost | — | Your requirement | Staging and localhost |
| Admin token | "short-lived" | 7 days | Code | Says 7 days |
| GET `/iclock/cdata` body | "GET OPTION FROM…" handshake | `OK` | Code and RULES §5 | `OK` |
| GET `/iclock/getrequest` body | empty | `OK` | Code | `OK` |
| ADMS `SN` and `table` | both required; `table` limited to 4 values | neither enforced; anything other than ATTLOG is acknowledged | Code | Optional; SN modelled as the device's credential |
| 403 status | only on the dashboard | wrong-role token on 20 routes | Code | Added |
| 413 / 431 / 500 | missing on 9 / 30 / 6 operations | returned | Code | Added |
| Admin login 400, and its Arabic 401 message | not documented | returned | Code | Added |
| `StudentPayload` | `rfid_tag` required; `id` optional; `parent_pin` shown in responses | `rfid_tag` optional; `id` required on update; `parent_pin` never returned | Code | Split into 3 schemas |
| `last_sync` | "ISO timestamp" | Baghdad `YYYY-MM-DD HH:MM:SS`, left out until the first sync | Code | Documented |
| Device create/update | `is_active` example is true | omitted means disabled; omitted location is cleared | Code (fix proposed) | Documented |
| Parent-app fields | nothing marked required | always present | Code | `required` lists added |
| Public settings | any key | only 2 keys, left out when unset | Code | Own schema |
| `is_read`, today's order, empty months | not described | always false; no defined order; `[]` | Code (fixes proposed) | Documented |
| Security | one shared scheme; 9 operations had none | parent, admin and device-serial checks | Code | 3 schemes; public operations marked public |
| Examples | the seeded admin password `admin123`, a real-looking device serial, possibly real phone numbers | — | — | Replaced with fake data |

**RULES.md:**
- §1 routing and middleware, §4 response helpers, §4 `DailyAttendanceDTO`, §5 ADMS and time windows, and §6 e2e all pass.
- §4 "the spec must match exactly" failed before and passes now.
- One three-way conflict: GET `/iclock/cdata`. RULES and the code say `OK`; the old spec showed a handshake body. I went with RULES and the code.

### Proposed Go fixes (not applied)
- **High:**
  - **FCM token handling:** the token can only be sent at login and is stored on each child's record. So only the last phone to log in gets pushes, a refreshed token is lost, a child added later gets no pushes, and logging out doesn't stop them. Needs an authenticated endpoint to update and delete the token.
  - **Admin login has no rate limit.**
- **Medium:**
  - Database constraint errors return 500 instead of a 4xx:
    - a duplicate `rfid_tag` (should be 409);
    - a leave for an unknown student (should be 404);
    - over-long values, including a PIN over 72 bytes (should be 400).
  - Year `0000` in a date or month passes validation and then fails in Postgres with a 500.
  - Changing a PIN doesn't sign out other phones.
  - Phone numbers are matched as exact strings, with no normalisation.
  - Device create/update treats omitted fields as "disable" or "clear".
- **Low:**
  - Today's attendance has no `ORDER BY`.
  - There's no way to mark a notification as read.
  - Unknown paths get a plain-text 404.
  - A wrong method gets 401 before 405.
  - Disabling an unknown device returns 200.
  - The admin login error message is in Arabic, while every other message is English.
  - The JSON device push never sends notifications.
  - Monthly records call the check-in time `check_time`, while daily records call it `check_in_time`.

### Could not verify
- **Staging** was never contacted (no Railway access, by your rule), so I can't confirm it's reachable or runs this version.
- **The Postman app itself:** I checked with the `openapi-to-postmanv2` converter instead. It produced 30 requests with 164 example responses; the Flutter-only bundle gives 25.
- **Real ZKTeco firmware:** whether a physical device accepts `OK` as the reply to its GET handshake.
- **Actual FCM delivery to phones:** in tests, Firebase points at a closed local port.
- **Railway's proxy:** its 502/503 bodies and how its 30-second timeout behaves.

Two notes:
- **Header limit:** Go allows up to 8 KiB beyond the configured 64 KiB, so 431 only starts at about 68–72 KiB. The spec now says so, and the test sends 128 KiB.
- **Lint:** Redocly lint is now at 0 errors and 0 warnings (it was 9 errors and 38 warnings). Two rules are switched off in `redocly.yaml`, each with a comment: `no-server-example.com` (you asked for the localhost entry) and `info-license-strict` (the licence is proprietary, with no public URL).

If you'd rather send the Flutter developer a link than the file, I can publish the handoff notes as a private page.
