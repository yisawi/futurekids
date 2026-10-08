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
3. **Log in:** send `POST /api/mobile/login` with `{"phone": "07000000101", "pin": "482193"}`.
   - `phone` is the parent's Iraqi mobile number, as typed. The server accepts:
     - `07XXXXXXXXX`, `+9647XXXXXXXXX`, `009647XXXXXXXXX` and `9647XXXXXXXXX` (an extra 0 after
       `964` is tolerated);
     - spaces, dashes, dots and parentheses anywhere;
     - Arabic-Indic (`٠٧٧٠…`) or Persian (`۰۷۷۰…`) digits.
   - No client-side formatting is needed. All formats are the same number, and the response's
     `data.parent.phone` is always `+9647XXXXXXXXX`. Anything else (a landline, a foreign number)
     gets the same 401 as an unregistered number.
   - `pin` is a string, sent exactly as typed. New PINs are 6 digits, but don't validate the
     format in the app: parents whose PIN was set earlier may have a different one, and login
     accepts it.
4. **Store the token:** keep `data.token` in secure storage, along with `data.parent`
   (`id`, `name`, `phone`). **Never store the PIN.**
5. **Authenticate every other parent call:** send `Authorization: Bearer <token>`. The word
   `Bearer` must be exactly that, followed by one space.
6. **Handle 401:** any parent call answering 401 means the token is no longer valid: it's missing
   or expired, the **school changed the parent's PIN** (which signs the parent out of every phone),
   or the account was removed. Delete the token and show the login screen. Tokens last **30 days**,
   and there is **no refresh endpoint**.
7. **Register for pushes:** right after login, call `PUT /api/mobile/device-token` (see FCM below).
8. **Log out:** call `DELETE /api/mobile/device-token` with this phone's FCM token **while you still
   hold the parent token**, then delete the stored token. There is no other server-side logout.

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
| `MonthlyRecord.check_in_time`, `check_out_time` | `null` when there's no punch in that window |
| `NotificationPage.next_before` | `null` on the last page |
| `PublicSettingsResponse.data.whatsapp_number`, `school_name` | **Left out** (not null) when the school hasn't set them |

These strings are never null but may be `""`:

- `MobileStudent.grade`, `section`, `avatar_url`
- `ScheduleEntry.teacher_name`
- `Banner.title`, `action_link`

## Dates, times and time zone

- **Time zone:** everything is **Asia/Baghdad (UTC+03:00, no daylight saving)**. "Today" and the
  default month are Baghdad's, not the phone's.
- **Year range:** every date and month the API accepts (query parameters and fields) must have a
  year from **2000 to 2100**; any other year returns 400.
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
| `GET /api/mobile/attendance/today` | By `student_id`, ascending |
| `GET /api/mobile/attendance/summary` | Children by `id`, ascending |
| `GET /api/mobile/attendance/monthly` | Children by `id`, ascending; each child's `records` newest day first (school days up to today) |
| `GET /api/mobile/schedule` | Child (`id` ascending), then **Sunday → Thursday, then Friday, Saturday**, then `period_number`. Unrecognized day names come last. |
| `GET /api/mobile/notifications` | Newest first |
| `GET /api/mobile/banners` | Newest first |

- **Schedule days:** `day_of_week` is the day name as stored. Schedules saved from the admin
  dashboard always use `الأحد`, `الإثنين`, `الثلاثاء`, `الأربعاء`, `الخميس`; rows the school
  entered earlier by other means may use other spellings (English, without hamza). It isn't an
  enum or a number. Group consecutive entries by `day_of_week` in the order received; don't sort
  or switch on English names.
- **Summary and monthly with no school days:** for a month with no school day up to today (a
  future month, or a month whose first days are Friday and Saturday), both return `"data": []`,
  not zero counts.

## Banners in the parent app

- **`GET /api/mobile/banners` returns every active banner, newest first.** Show them as a
  carousel and load each picture lazily, when its page is about to appear.
- **Loading a picture:** when `image_url` starts with `/`, it's relative to the API base URL:
  load `baseUrl + image_url`, for example
  `Image.network('$baseUrl/api/mobile/banners/image?id=5')`. **No `Authorization` header is
  needed** for it. Any other `image_url` is a full URL from an older banner; load it as is.
- **Caching:** the picture comes with an `ETag` and `Cache-Control: public, max-age=300`. A cached
  copy may be reused for 5 minutes; after that, revalidating with `If-None-Match` returns `304`
  while it's unchanged. Changing the picture changes the `ETag`.
- **404 on a picture:** the banner was deactivated or deleted since the list was loaded. Skip it
  and refresh the list later.

## Notifications pagination

`GET /api/mobile/notifications` returns at most **100** items per page, newest first:

```json
{"status": "success", "data": [...], "has_more": true, "next_before": 1402, "unread_count": 3}
```

1. **First page:** call without `before`. Pull-to-refresh does the same.
2. **Next page:** while `has_more` is `true`, call `?before=<next_before>`.
3. **Last page:** `has_more` is `false` and `next_before` is `null`.

Pages are keyed on the notification id, so new notifications arriving while you scroll don't
shift or duplicate items. A `before` that isn't a positive integer returns 400.

### Read state and the unread badge

- **`is_read`** is stored on the server and is the same on every phone of the parent. New
  notifications always arrive unread.
- **`unread_count`** is the parent's total number of unread notifications, across all pages. Use
  it for the badge (for example on the bell icon). It's optional to use, but don't count
  `is_read` on the pages you've loaded: older pages may hold more unread items.
- **Mark one:** call `PUT /api/mobile/notifications/read?id=<id>` (no body) when the parent opens
  or taps a notification.
  - Returns 200, also when it was already read.
  - Returns 404 when the id isn't one of this parent's notifications.
  - Returns 400 for an id that isn't a positive integer.
- **Mark all:** call `PUT /api/mobile/notifications/read-all` (no body) for a "mark all as read"
  action. It returns `{"data": {"updated": <how many changed>}}`; 0 means nothing was unread.
- **After either call,** refresh the list or update the badge from its next `unread_count`. A push
  that arrives later is unread again.

### History belongs to the account

History follows the parent account, not the phone number: it survives a change of the parent's
number and is deleted only with the account.

## Push notifications (FCM)

Each parent can have up to **10 phones** registered. Every push about any of the parent's
children goes to all of them, including children the school adds later.

**The flow:**

1. **After every successful login**, get the token with `FirebaseMessaging.instance.getToken()` and
   send `PUT /api/mobile/device-token` with `{"token": "<fcm token>"}` and the parent token.
2. **At every app start** while signed in, send the same `PUT` again. It's idempotent, and it
   marks the phone as recently used, which protects it from the 10-phone limit.
3. **On `FirebaseMessaging.instance.onTokenRefresh`**, `PUT` the new token. The old one is
   removed automatically the next time FCM rejects it. You can also `DELETE` it if the app kept it.
4. **On logout**, first `DELETE /api/mobile/device-token` with `{"token": "<fcm token>"}` and the
   parent token, then discard the parent token, then optionally `deleteToken()`. The `DELETE`
   needs the parent token, so do it before discarding it.

**Responses:**

- **200** for both calls, including when the token wasn't registered to this parent. A retried
  logout is safe.
- **400** when the token isn't 20–1024 characters of letters, digits, `:`, `_`, `.` or `-`.
  Real FCM tokens always fit.
- **401 / 403** are handled as for any parent call: sign in again.

**Situations the app should expect:**

- **Reinstall:** the app gets a new FCM token and must log in again, which registers the new token.
  The old token stops working at FCM and the server drops it on the next push.
- **Another parent signs in on the same phone:** registering moves the token to that parent, so the
  phone only receives that parent's pushes.
- **PIN change by the school:** removes all of the parent's phones; they get 401 and must log in
  and register again.
- **Login compatibility:** `fcm_token` in the login body still works, but use the dedicated
  endpoints, which are the contract.

**What gets sent:**

- A check-in push for a child's first punch in the morning window.
- A check-out push for the first punch in the afternoon window.
- An absence push at 12:00 Baghdad time, Sunday to Thursday, for each child still `Absent`.
- One push per child per event, to every registered phone of the parent.

**Payload and history:**

- **Payload:** **notification only** (Arabic `title` and `body`). There's **no `data` payload**, so
  there are no ids or types to deep-link on. On tap, open the notifications screen and refresh it.
- **History:** every notification is also saved to the history list, even when no push could be
  delivered.

## Admin dashboard

- **PIN for a new parent:** `POST`/`PUT /api/admin/students` with a `parent_phone` that isn't
  registered yet **needs `parent_pin`**.
  - Missing, empty or only spaces → 400 `parent_pin is required for a new parent …`. The server
    never makes up a PIN.
  - **Format:** exactly **6 ASCII digits** (`0-9`). Anything else, including Arabic-Indic or
    Persian digits and spaces, gets 400 `parent_pin must be exactly 6 digits (0-9)`.
  - **Generate it with a cryptographically secure random source.** In Dart that's
    `Random.secure()`, e.g. `List.generate(6, (_) => Random.secure().nextInt(10)).join()`. Never
    use `Random()` or anything derived from the phone number or date.
  - **Show it before saving:** hidden, with a reveal toggle and a copy button, so the admin can
    hand it to the parent.
  - **The server can't show it again.** It stores only a hash and never returns the PIN. If it's
    lost, set a new one, which also signs the parent out of every phone.
  - **Existing parents:** leave `parent_pin` out to keep the current PIN. Sending one replaces it
    and signs that parent out of every phone.
- **409 on a student create or update:** another student already uses that `rfid_tag` (the device
  User ID). Nothing was saved. Show "this device ID is already used by another student" and let
  the admin choose another.
- **404 on `POST /api/admin/leaves`:** the student doesn't exist or was deactivated. Refresh the
  student list.
- **Devices:**
  - A device registered without `is_active` is active.
  - `PUT /api/admin/devices` changes only the fields you send (`location_name`, `is_active`);
    omitted ones keep their value.
  - Send at least one field (400 otherwise). Sending `"location_name": ""` clears the location.
- **Length limits** (400 names the field and the limit):

  | Field | Maximum |
  |---|---|
  | `name` | 100 characters |
  | `parent_name` | 255 |
  | `rfid_tag`, `grade`, `section` | 50 |
  | device `serial_number`, `location_name` | 50 |
  | setting `key` | 100 |
  | `parent_pin` | exactly 6 digits |

  Setting values and leave notes have no limit beyond the 64 KiB request body.
- **Dates:** report and leave dates, and the parent's months, accept years 2000 to 2100 only.
- **Grade and section** of a student are stored with surrounding spaces removed, so `" G3 "` is
  saved as `G3`. Matching is otherwise exact and case-sensitive (`G3` and `g3` are different
  classes).

### Weekly schedule

One screen edits the schedule of one class (a grade and section pair):

1. **Choose the class** from the distinct `grade` and `section` values in
   `GET /api/admin/students`. There's no class-list endpoint.
2. **Load it** with `GET /api/admin/schedule?grade=G3&section=A`. A class without a schedule
   returns 200 with `"data": []`.
3. **Edit the grid** locally: Sunday to Thursday, periods 1 to 12, each cell a subject and an
   optional teacher.
4. **Save the whole class with one `PUT /api/admin/schedule`:**
   ```json
   {"grade": "G3", "section": "A", "periods": [
     {"day_of_week": "الأحد", "period_number": 1, "subject_name": "Mathematics", "teacher_name": "Teacher Example"},
     {"day_of_week": "الأحد", "period_number": 2, "subject_name": "Science", "teacher_name": null}
   ]}
   ```
   It replaces every period of that class; cells you leave out are removed. Other classes never
   change. The response has the same shape as the `GET`, with the schedule as now stored; show it
   instead of your local copy.

**Rules:**

- **Empty save:** `"periods": []` clears that class's schedule. Ask the admin to confirm first.
  Leaving `periods` out (or `null`) is a 400, so a bug can't clear a class.
- **Day names:** send the Arabic names (with or without hamza) or the English names in any case.
  The server stores and returns `الأحد`, `الإثنين`, `الثلاثاء`, `الأربعاء`, `الخميس`. Friday,
  Saturday or anything else is 400.
- **Limits:**
  - `period_number` is 1 to 12.
  - `subject_name` is required, 1 to 100 characters.
  - `teacher_name` is optional, up to 100 characters; `null` or blank is saved as `null`.
  - `grade` and `section` are required, 1 to 50 characters, and saved with surrounding spaces
    removed.
  - At most 60 periods per request (5 days × 12), and each day and period pair at most once.
- **Errors:** a 400 `message` names the problem, and a period by its 0-based index, for example
  `periods[2].day_of_week must be a school day, Sunday to Thursday (Arabic or English)`. A failed
  save (400 or 500) changes nothing; the stored schedule stays as it was.
- **Two admins saving the same class at once:** one save wins entirely; the schedule is never a mix.
- **What parents see:** `GET /api/mobile/schedule` shows these periods to every active child whose
  `grade` and `section` are equal to the saved ones, in the same order. A missing teacher appears
  there as `""` (here it's `null`).

### Banners

The school's announcements, shown to parents as a carousel. Every picture is uploaded from the
dashboard; there are no pasted URLs.

1. **List:** `GET /api/admin/banners` returns every banner, active or not, newest first, with
   `image_content_type` and `image_size_bytes` but never the picture itself.
2. **Upload:** pick the picture with `image_picker`, **resize and compress it before uploading**
   (at most 1600 px wide, about 80 % JPEG quality; that's comfortably under the 2 MiB limit),
   then `POST /api/admin/banners` as `multipart/form-data`:

   | Part | Content | Rules |
   |---|---|---|
   | `image` | the file | Required. JPEG, PNG or WebP, at most 2 MiB (2,097,152 bytes) |
   | `title` | text | Optional, at most 255 characters; blank means none |
   | `action_link` | text | Optional, at most 2048 characters; an absolute `http`/`https` URL; blank means none |
   | `is_active` | `true` or `false` | Optional, default `true` |

   The response is the stored banner. Show the picture to the admin before saving, from the
   local file.
3. **Edit:** `PUT /api/admin/banners?id=<id>`, also `multipart/form-data`. Send only the parts
   you change; the others keep their value. An `image` part replaces the picture. A blank
   `title` or `action_link` clears it. Sending no part is 400.
4. **Activate or deactivate:** `PUT …?id=<id>` with only `is_active=true` or `is_active=false`.
   A deactivated banner disappears from the parent app but stays in this list.
5. **Delete:** `DELETE /api/admin/banners?id=<id>` removes the banner and its picture for good.
   Ask the admin to confirm; to hide one temporarily, deactivate it instead.
6. **Preview:** `GET /api/admin/banners/image?id=<id>` serves the picture of any banner, active
   or not. It **needs the admin token**:
   `Image.network(url, headers: {'Authorization': 'Bearer $adminToken'})`. Older banners without
   an uploaded picture return 404 there; show their `image_url` instead.

**Rules:**

- **The server checks the picture's bytes,** not the file name or the part's Content-Type. SVG,
  GIF, BMP, HEIC and anything else get 400 `image must be a JPEG, PNG or WebP picture`.
  `image_picker` may return HEIC on iPhones, so re-encode to JPEG before uploading.
- **Too large:** a picture over 2 MiB gets 413 `image must be at most 2 MiB (2097152 bytes)`; a
  whole request over 2 MiB + 64 KiB gets 413 `Request body too large`.
- **Errors change nothing:** a 400, 413 or 500 leaves the banner exactly as it was. `message`
  names the problem, for example `action_link must be an absolute http or https URL`.
- **404:** the banner was deleted meanwhile; refresh the list.
- **Slow networks:** an upload may take up to 2 minutes before the server gives up.

## Don't assume

- **Tokens belong to the account, not to one child or one phone.** A parent's phones all receive
  every child's pushes. Don't assume one token per parent, and don't send per-child tokens.
- **The server never tells you which phones are registered.** There's no list endpoint. Keep this
  phone's token locally if you want to `DELETE` it later.
- **More than 10 phones:** the phone seen least recently stops receiving pushes until it registers again.
- **Read state is per account, not per phone:** marking a notification read on one phone marks it
  read on all of the parent's phones.
- **`unread_count` can change between calls** (new pushes, another phone marking items read).
  Take it from the latest list response; don't calculate it yourself.
- **A valid token isn't proof the account still has children:** a parent with no active child gets
  `200` with empty lists, not an error.
- **Don't keep using a token after a 401:** once the school changes the PIN, every phone signed in
  with the old PIN gets 401, and only a new login (with the new PIN) works again.
- **URLs aren't validated:** `avatar_url`, and the `image_url` of banners entered before uploads
  existed, are whatever the school entered, possibly empty. Uploaded banners always have a
  working relative `image_url`.
- **Ids are 32-bit integers.** No id is a string.

## Breaking changes since earlier drafts

**In API 1.9.0 (this release):**

- **New:** banner management for the dashboard (`GET`, `POST`, `PUT`, `DELETE /api/admin/banners`
  and `GET /api/admin/banners/image`), and the public picture route `GET
  /api/mobile/banners/image`. See [Banners](#banners) and
  [Banners in the parent app](#banners-in-the-parent-app).
- **`image_url` of an uploaded banner is relative** (`/api/mobile/banners/image?id=<id>`); prefix
  the API base URL. The response shape of `GET /api/mobile/banners` is unchanged, and banners
  entered earlier keep their full URL.

**In API 1.8.0:**

- **New:** `GET` and `PUT /api/admin/schedule` (see [Weekly schedule](#weekly-schedule)). Nothing
  existing was removed or renamed.
- **Student `grade` and `section` are trimmed:** `POST`/`PUT /api/admin/students` store them
  without surrounding spaces (a value of only spaces is stored as `""`). Before, spaces were kept,
  so the student never matched a schedule. Students saved earlier keep their stored values until
  they're saved again.

**In API 1.7.0:**

- **`parent_pin` must be exactly 6 ASCII digits** whenever the admin sets or replaces it (400
  otherwise). In 1.6.0 any value up to 72 bytes was accepted. Login doesn't check the format,
  so PINs set earlier keep working.

**In API 1.6.0:**

- **No default PIN any more:** creating a student for a new parent without `parent_pin` returns
  400; earlier versions silently gave the parent a fixed default PIN. The dashboard must generate
  and send the PIN.
- **Duplicate `rfid_tag`** on a student create or update returns **409** (it was a 500).
- **A leave for an unknown or deactivated student** returns **404** (an unknown student was a
  500; a deactivated one was accepted).
- **Device update:** `PUT /api/admin/devices` keeps omitted fields. Before, an omitted
  `location_name` was cleared and an omitted `is_active` disabled the device. Sending neither
  field is now 400.
- **Device create:** `is_active` defaults to `true`; it used to default to `false`.
- **Over-long values** return 400 naming the field (they were 500s).
- **Dates and months outside 2000–2100** return 400 (year 0000 was a 500; other years were
  accepted).
- **Ordering:** parent today attendance is ordered by `student_id`, and the admin daily report
  breaks name ties by `student_id`.

**In API 1.5.0:**

- **Monthly records renamed `check_time` to `check_in_time`.** Earlier copies of the spec used
  `check_time` for the check-in time in the records of `GET /api/mobile/attendance/monthly`. The
  daily records (admin report and parent today) and the monthly records now share the same names,
  `check_in_time` and `check_out_time`, with the same values, `null` and `hh:mm AM` format, so one
  model fits both. Nothing else in the monthly response changed.

**In API 1.4.0:**

- **`is_read` is no longer always `false`:** it's the real read state, set by the new `PUT`
  `/api/mobile/notifications/read?id=` and `/api/mobile/notifications/read-all`.
- **The notification list adds `unread_count`** (required, the total of unread notifications). The
  rest of the list response is unchanged.
- **History is keyed by the parent account,** so it survives a change of the parent's phone number.

**In API 1.3.0:**

- **Push tokens are per parent device:** new `PUT` and `DELETE /api/mobile/device-token`.
  - Every registered phone of a parent gets every push. Before, only the last phone to log in did,
    and only for children that existed at that login.
  - Logout now stops pushes (`DELETE`), and a refreshed token can be sent (`PUT`).
- **A PIN change also removes the parent's registered phones.**
- **Login's `fcm_token`:** an invalid token is now ignored (the login still succeeds) instead of
  stored.

**In API 1.2.0:**

- **Every parent must log in once after the upgrade.** Tokens issued before 1.2.0 get 401.
- **Changing a PIN signs the parent out of every phone** (401); before, old tokens kept working.
- **Phone numbers are normalised.** The server accepts the formats above and always returns
  `+9647XXXXXXXXX`. An admin creating a student with a non-Iraqi-mobile `parent_phone` gets 400.
- **A token for a deleted parent** gets 401; it used to get 200 with empty lists.
- **Unknown paths** return JSON 404 (was plain text).
- **Wrong methods** return JSON 405 with `Allow`, before authentication (it used to be 401 first
  for protected routes).
- **Admin login** can return 429 with `Retry-After`.