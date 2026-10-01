# Future Kids API — Flutter handoff

The contract is [`future_kids_api.yaml`](../future_kids_api.yaml) (OpenAPI 3.0). Build against the
operations tagged **Parent App**. Operations tagged **Hardware (internal)** are for the school's
attendance devices only; ignore them.

- **Postman:** Import → OpenAPI 3.0 → `future_kids_api.yaml`, then set the `baseUrl` and `bearerToken`
  variables. Postman uses one `bearerToken` variable for both parent and admin requests.
- **Spec without the hardware operations:** `npx @redocly/cli bundle flutter -o flutter_api.yaml`.

## Base URLs

| Environment | URL |
|---|---|
| Staging | `https://futurekids-staging.up.railway.app` |
| Local | `http://localhost:8080` |

Production is shared separately at release. Don't hard-code URLs; make the base URL configurable.

## Sign-in, step by step

1. **Before login (optional):** call `GET /api/mobile/settings` to get `school_name` and `whatsapp_number`
   for the login screen. It needs no token.
2. **Get the device's FCM token:** `FirebaseMessaging.instance.getToken()`.
3. **Log in:** send `POST /api/mobile/login` with `{"phone": "07000000101", "pin": "4821", "fcm_token": "<fcm token>"}`.
   - `phone` is the parent's Iraqi mobile number, as typed. The server accepts:
     - `07XXXXXXXXX`, `+9647XXXXXXXXX`, `009647XXXXXXXXX` and `9647XXXXXXXXX` (an extra 0 after
       `964` is tolerated);
     - spaces, dashes, dots and parentheses anywhere;
     - Arabic-Indic (`٠٧٧٠…`) or Persian (`۰۷۷۰…`) digits.
   - No client-side formatting is needed. All formats are the same number, and the response's
     `data.parent.phone` is always `+9647XXXXXXXXX`. Anything else (a landline, a foreign number)
     gets the same 401 as an unregistered number.
   - `pin` is a string.
4. **Store the token:** keep `data.token` in secure storage, along with `data.parent`
   (`id`, `name`, `phone`). **Never store the PIN.**
5. **Authenticate every other parent call:** send `Authorization: Bearer <token>`. The word
   `Bearer` must be exactly that, followed by one space.
6. **Handle 401:** any parent call answering 401 means the token is no longer valid: it's missing
   or expired, the **school changed the parent's PIN** (which signs the parent out of every phone),
   or the account was removed. Delete the token and show the login screen. Tokens last **30 days**,
   and there is **no refresh endpoint**.
7. **Log out:** there is no server logout. Delete the stored token and call
   `FirebaseMessaging.instance.deleteToken()`, so this phone stops getting that parent's pushes
   (see FCM below).

## Errors

Every JSON error has this shape:

```json
{"status": "error", "message": "Invalid or expired token"}
```

Decide what to do from the **HTTP status**. `message` is for logs and may change.

Some responses are **not JSON**: 431, and 502/503 from Railway's edge proxy during a deploy.
Handle a non-JSON body without crashing.

| Status | When | What the app does |
|---|---|---|
| 200 | Success | — |
| 400 | Bad input: login without `phone`/`pin`, a malformed `month`, a bad `before` cursor | Fix the request. Show a generic error. |
| 401 | **Login:** wrong phone or PIN, or not an Iraqi mobile number. **Anything else:** no valid token, including after a PIN change. | Login: "wrong phone or PIN". Otherwise log out and show login ("please sign in again"). |
| 403 | The token isn't a parent token (for example an admin token) | Log out and show login. |
| 404 | Unknown path (JSON) | App/server version mismatch. Report it. |
| 405 | Wrong HTTP method (JSON, with an `Allow` header). Decided before authentication. | App bug. |
| 413 | Request body over 64 KiB | Never happens with normal requests. |
| 429 | **Login only.** Parent: 5 failed attempts for this phone number (any format) within 15 minutes. Admin: 5 failures for a username from one network, or 20 from one network. | Read `Retry-After` (seconds). Show "try again in N minutes" and disable the button. **Don't auto-retry.** The right PIN or password is also refused until the wait ends. An admin locked out on one network can still log in from another. |
| 431 | Request headers over 64 KiB (plain text) | Never happens with normal requests. |
| 500 | Server or database error | Show a generic error with a retry button. |
| 502 / 503 | Railway edge during a deploy or outage (not the API's JSON) | Retry with backoff. |
| no response | A request running more than 30 s is dropped | Retry with backoff. |

## Nullable and optional fields

Everything in parent responses is always present and non-null, **except** the fields below. The
spec's `required` and `nullable` lists match what the server sends; the contract tests check this.

| Field | Meaning |
|---|---|
| `DailyAttendanceDTO.check_in_time`, `check_out_time` | `null` when there's no punch in that window |
| `MonthlyRecord.check_time`, `check_out_time` | `null` when there's no punch in that window (`check_time` means check-in) |
| `NotificationPage.next_before` | `null` on the last page |
| `PublicSettingsResponse.data.whatsapp_number`, `school_name` | **Left out** (not null) when the school hasn't set them |

These strings are never null but may be `""`:

- `MobileStudent.grade`, `section`, `avatar_url`
- `ScheduleEntry.teacher_name`
- `Banner.title`, `action_link`

## Dates, times and time zone

- **Time zone:** everything is **Asia/Baghdad (UTC+03:00, no daylight saving)**. "Today" and the
  default month are Baghdad's, not the phone's.
- **Formats:**

  | Value | Format |
  |---|---|
  | `date` | `YYYY-MM-DD` |
  | `month` (query parameter and response) | `YYYY-MM` |
  | Punch times | `hh:mm AM` / `hh:mm PM`, e.g. `07:15 AM`: Baghdad wall-clock with no zone. Show as-is; **don't convert.** |
  | `created_at` (notifications) | RFC 3339 with `+03:00`, e.g. `2026-09-21T07:15:04+03:00`. `DateTime.parse` handles it. |

- **School week:** Sunday to Thursday. Friday and Saturday never appear in monthly data.
- **Punch windows:** a punch counts only in the **check-in window (06:30–09:30)** or the
  **check-out window (11:30–13:30)**.
- **Status:**
  - `Present` means at least one punch in either window.
  - `Excused` means no punch and a leave recorded for the day.
  - `Absent` means neither. That includes a child who **hasn't arrived yet today** and a child who
    punched only outside the windows (for example at 10:00).

## Lists and ordering

| Endpoint | Order |
|---|---|
| `GET /api/mobile/students` | By `id`, ascending |
| `GET /api/mobile/attendance/today` | **Not guaranteed.** Match records to children by `student_id`. |
| `GET /api/mobile/attendance/summary` | Children by `id`, ascending |
| `GET /api/mobile/attendance/monthly` | Children by `id`, ascending; each child's `records` newest day first (school days up to today) |
| `GET /api/mobile/schedule` | Child (`id` ascending), then **Sunday → Thursday, then Friday, Saturday**, then `period_number`. Unrecognized day names come last. |
| `GET /api/mobile/notifications` | Newest first |
| `GET /api/mobile/banners` | Newest first |

- **Schedule days:** `day_of_week` is the day name **as the school typed it**, usually Arabic
  (`الأحد`), sometimes English. It isn't an enum or a number. Group consecutive entries by
  `day_of_week` in the order received; don't sort or switch on English names.
- **Summary and monthly with no school days:** for a month with no school day up to today (a
  future month, or a month whose first days are Friday and Saturday), both return `"data": []`,
  not zero counts.

## Notifications pagination

`GET /api/mobile/notifications` returns at most **100** items per page, newest first:

```json
{"status": "success", "data": [...], "has_more": true, "next_before": 1402}
```

1. **First page:** call without `before`. Pull-to-refresh does the same.
2. **Next page:** while `has_more` is `true`, call `?before=<next_before>`.
3. **Last page:** `has_more` is `false` and `next_before` is `null`.

Pages are keyed on the notification id, so new notifications arriving while you scroll don't
shift or duplicate items. A `before` that isn't a positive integer returns 400.

`is_read` is **always `false`**: no endpoint marks notifications as read. Track read state on the
device by notification `id`.

## Push notifications (FCM)

- **Registering a token:** the only way to send the token is the `fcm_token` field of
  `POST /api/mobile/login`. Send it on every login.
- **What gets sent:**
  - A check-in push when a child's first punch of the morning window arrives.
  - A check-out push for the first punch of the afternoon window.
  - An absence push at 12:00 Baghdad time, Sunday to Thursday, for each child still `Absent`.
  - One push per child per event.
- **Payload:** **notification only** (Arabic `title` and `body`). There's **no `data` payload**, so
  there are no ids or types to deep-link on. On tap, open the notifications screen and refresh it.
- **History:** every notification is also saved to the history list, even when no push could be
  delivered.

## Don't assume

- **Pushes reach only one device per parent.** The token is stored on each child's record, so the
  most recent login wins. If both parents use the same account on two phones, only the last phone
  to log in gets pushes.
- **A refreshed FCM token doesn't reach the server** until the next login. There's no endpoint to
  update it yet, and the app must not keep the PIN to log in silently.
- **A child added after login gets no pushes** until the parent logs in again (history still records them).
- **Notifications are keyed by the parent's phone number,** not the parent id. History follows the phone number.
- **Logging out doesn't stop pushes on the server side.** Call `deleteToken()` on logout.
- **A valid token isn't proof the account still has children:** a parent with no active child gets
  `200` with empty lists, not an error.
- **Don't keep using a token after a 401:** once the school changes the PIN, every phone signed in
  with the old PIN gets 401, and only a new login (with the new PIN) works again.
- **URLs aren't validated:** `avatar_url` and banner `image_url` are whatever the school entered, possibly empty.
- **Ids are 32-bit integers.** No id is a string.

## Breaking changes since earlier drafts

**In API 1.2.0 (this release):**

- **Every parent must log in once after the upgrade.** Tokens issued before 1.2.0 get 401.
- **Changing a PIN signs the parent out of every phone** (401); before, old tokens kept working.
- **Phone numbers are normalised.** The server accepts the formats above and always returns
  `+9647XXXXXXXXX`. An admin creating a student with a non-Iraqi-mobile `parent_phone` gets 400.
- **A token for a deleted parent** gets 401; it used to get 200 with empty lists.
- **Unknown paths** return JSON 404 (was plain text).
- **Wrong methods** return JSON 405 with `Allow`, before authentication (it used to be 401 first
  for protected routes).
- **Admin login** can return 429 with `Retry-After`.

**Earlier:**

- **`/health` returns JSON:** `{"status": "success", "message": "..."}`; it used to be plain text.
- **Punch times are always present:** `check_in_time`/`check_out_time` (today and admin report) and
  `check_time`/`check_out_time` (monthly) are sent as **`null`** when there's no punch. They used to
  be left out of the JSON.
- **Notifications are paginated:** at most 100 per page, plus `has_more` and `next_before`, with a
  `?before` cursor. The endpoint used to return the full history.
- **`/api/mobile/settings` is restricted:** it returns only `whatsapp_number` and `school_name`; it
  used to return every setting.
- **Login can return 429** with `Retry-After` after 5 failed attempts in 15 minutes.
- **Newly documented errors:**
  - 403 on every parent endpoint for a non-parent token;
  - 413 for bodies over 64 KiB;
  - 431 for oversized headers.
- **Summary counting:** it counts **Sunday–Thursday**; the first draft said Monday–Thursday.
- **Schedule order is defined** (school week, then period); it used to be alphabetical by day name.
- **Spec changes that affect generated code:**
  - Response wrappers now have schema names: `MobileStudentListResponse`,
    `AttendanceSummaryResponse`, `MonthlyAttendanceResponse`, `ScheduleResponse`,
    `NotificationPage`, `BannerListResponse` and `PublicSettingsResponse`.
  - Fields are now marked `required`, so generated models are non-nullable except for the
    fields listed above.
  - The security scheme `BearerAuth` is now `ParentJWT` (parent) and `AdminJWT` (admin).
  - The server list now has Staging instead of Production.
