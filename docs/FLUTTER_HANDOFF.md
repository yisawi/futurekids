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
   - `pin` is the parent's credential, a string sent **exactly as typed**: don't trim it and don't
     check its format. New credentials are 16 characters with letters, digits and symbols (for
     example `Kq7#vR2m!Tx9pW4z`), but older ones of 4 or 6 digits still exist and login accepts
     them.
   - **The login field is a normal text field**, not a numeric keypad: an obscured text field with
     a show/hide toggle, paste allowed, autocorrect and suggestions off, no capitalisation.
   - People on an Arabic keyboard must switch layouts to type Latin letters and symbols, which is
     why the dashboard uses a small symbol set. The token lasts **30 days**, so typing it is rare.
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

Decide what to do from the **HTTP status**. `message` is for logs and may change. The one
extension: a 400 from the admin Excel import (`POST /api/admin/schedule/import`) can add an
`errors` list (see [Weekly schedule](#weekly-schedule)).

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
- **"Not arrived yet" before 09:31:** for **today**, an `Absent` status before **09:31 Asia/Baghdad**
  (while the check-in window is open) means the child hasn't arrived yet, not that they're absent.
  Show "not arrived yet" wording on the parent's today screen (`GET /api/mobile/attendance/today`)
  and in the admin daily report and dashboard. Compare against the current time in
  **Asia/Baghdad** (UTC+03:00), never the phone's time zone. From 09:31:00 `Absent` means absent.
  The monthly records and the summary already leave an `Absent` today out until then.
- **A student's calendar starts on the day they were added.** Earlier days aren't returned or
  counted: the monthly records may start after the 1st, the summary counts only days since then,
  and a month entirely before that day leaves the child out (`"data": []` when no child has a
  day). Draw the missing days like Friday and Saturday, as days without a record, not as absences.
- **Past-date admin reports** (`GET /api/admin/attendance?date=` and the Excel export) list only
  students who already existed on that date; students added later aren't listed.

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
  dashboard (screen or Excel import) always use `الأحد`, `الإثنين`, `الثلاثاء`, `الأربعاء`, `الخميس`; rows the school
  entered earlier by other means may use other spellings (English, without hamza). It isn't an
  enum or a number. Group consecutive entries by `day_of_week` in the order received; don't sort
  or switch on English names.
- **Summary and monthly with no counted day:** a child with no counted day in the month is left
  out. For a month where no child has one (a future month, a month before the children were
  added, or a month whose first days are Friday and Saturday), both return `"data": []`, not zero
  counts.

## Weekly schedule in the parent app

`GET /api/mobile/schedule` returns one flat list for all of the parent's children: by child (`id`
ascending), then Sunday to Thursday, then `period_number`.

- **Group** the items by `student_id`, then by `day_of_week` in the order received (Sunday to
  Thursday). Don't sort by the day name.
- **Header:** show the child's `student_name`, `grade` and `section` from the items.
- **Periods chip:** count the day's items and show "N periods" (5 or 6 for schedules saved since
  API 1.13.0; older ones may differ).
- **Teacher:** `teacher_name` is `""` when not set; hide the teacher line then.
- **Icon:** pick it from `subject_key`, never from the Arabic subject name. Map unknown values to
  the `other` icon, since keys may be added later. The app owns the icon assets; suggested
  concepts:

  | `subject_key` | Subjects | Icon concept |
  |---|---|---|
  | `math` | الرياضيات, الحساب | calculator |
  | `pe` | التربية الرياضية, التربية البدنية | ball or running figure |
  | `arabic` | اللغة العربية, القراءة | Arabic letter or open book |
  | `english` | اللغة الإنكليزية | "Aa" or speech bubble |
  | `islamic` | التربية الإسلامية, القرآن الكريم | crescent or mosque |
  | `computer` | الحاسوب | laptop |
  | `science` | العلوم | flask |
  | `social` | الاجتماعيات, التاريخ, الجغرافية, التربية الوطنية | globe |
  | `art` | التربية الفنية, الرسم | palette or brush |
  | `music` | الموسيقى, الأناشيد | music note |
  | `other` | anything else | generic book (default) |

- A child whose grade and section match no saved schedule has no items: show "no schedule yet".

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

Announcements the school sends from the dashboard arrive in this same list, unread, like any
other notification.

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
    never makes one up.
  - **Format:** **16 to 72 printable ASCII characters** (no spaces, nothing outside `!` to `~`)
    with **at least one letter, one digit and one symbol**. Anything else gets 400 `parent_pin
    must be 16 to 72 characters (ASCII, no spaces) with at least one letter, one digit and one
    symbol`.
  - **Generate 16 characters with a cryptographically secure random source:** `Random.secure()` in
    Dart, never `Random()` or anything derived from the phone number or date. Draw from a set
    without look-alikes and guarantee each kind by construction, for example:
    ```dart
    const letters = 'ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz'; // no I, l, O, o
    const digits = '23456789';                                          // no 0, 1
    const symbols = '!@#\$%&*?-_';
    String newParentCredential() {
      final r = Random.secure();
      String pick(String set) => set[r.nextInt(set.length)];
      const all = letters + digits + symbols;
      final chars = [pick(letters), pick(digits), pick(symbols),
        for (var i = 0; i < 13; i++) pick(all)]..shuffle(r);
      return chars.join();
    }
    ```
  - **Show it before saving:** hidden, with a reveal toggle and a copy button, so the admin can
    hand it to the parent.
  - **The server can't show it again.** It stores only a hash and never returns it. If it's lost,
    set a new one, which also signs the parent out of every phone.
  - **Existing parents:** leave `parent_pin` out to keep the current credential, even an older 6-
    digit one. Sending one replaces it and signs that parent out of every phone.
- **409 on a student create or update:** another student already uses that `rfid_tag` (the device
  User ID). Nothing was saved. Show "this device ID is already used by another student" and let
  the admin choose another.
- **404 on `POST /api/admin/leaves`:** the student doesn't exist or was deactivated. Refresh the
  student list.
- **Cancel a leave:** the daily report (`GET /api/admin/attendance?date=…`) shows a student with
  a leave as `Excused`. To undo a leave entered by mistake, call
  `DELETE /api/admin/leaves?student_id=<id>&date=<YYYY-MM-DD>` (no body).
  - The answer is always 200 `No leave remains for this student on this date`, also when there
    was nothing to cancel, so a retry is safe. Don't treat it as proof that a leave existed.
  - The student is then `Absent` for that day, unless they punched in a window (then `Present`).
    Reload the report to show it. If it's today and before 12:00, the noon absence notification
    reaches the parent again.
  - 400 when `student_id` isn't a positive integer or `date` isn't a real `YYYY-MM-DD` with a year
    from 2000 to 2100.
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
  | `parent_pin` | 16 to 72 ASCII characters, with a letter, a digit and a symbol |

  Setting values and leave notes have no limit beyond the 64 KiB request body.
- **Dates:** report and leave dates, and the parent's months, accept years 2000 to 2100 only.
- **Grade and section** of a student are stored with surrounding spaces removed, so `" G3 "` is
  saved as `G3`. Matching is otherwise exact and case-sensitive (`G3` and `g3` are different
  classes).

### Weekly schedule

Every class (a grade and section pair) has its own weekly schedule. The dashboard lists the
classes, and each class is edited on its own: on a screen, or through an Excel file that holds
that one class.

**The 5-or-6 rule (since API 1.13.0).** A day that has periods has **exactly 5 or 6**, numbered
**1 to 5 or 1 to 6**: no gaps, no repeats, nothing above 6. A day can be left out entirely (no
lessons that day), and different days of one class can have 5 or 6. Anything else is a 400 that
names the day. In the editing grid, show six rows per day and let the admin leave the sixth row
empty: a day with 5 periods simply has no period 6. Don't send a day with only some periods
filled; check the rule before saving and point at the day.

**Screen flow (one class):**

1. **Overview:** `GET /api/admin/schedule/classes` lists every known class: the grade and section
   pairs of active students together with every class that has a schedule, each with
   `student_count`, `period_count` and `day_count`. It's ordered by the stored strings; sort for
   display yourself. Show warnings from the counts:
   - `student_count` 0: "no students match this class, parents won't see this schedule" (usually a
     typo in the grade or section);
   - `period_count` 0: "no schedule yet".
2. **Open a class** with `GET /api/admin/schedule?grade=G3&section=A`. A class without a schedule
   returns 200 with `"data": []`.
3. **Edit the grid** locally: Sunday to Thursday, periods 1 to 6, each cell a subject and an
   optional teacher.
4. **Save the whole class with one `PUT /api/admin/schedule`:**
   ```json
   {"grade": "G3", "section": "A", "periods": [
     {"day_of_week": "الأحد", "period_number": 1, "subject_name": "الرياضيات", "teacher_name": "معلم تجريبي 1"},
     {"day_of_week": "الأحد", "period_number": 2, "subject_name": "العلوم", "teacher_name": null},
     {"day_of_week": "الأحد", "period_number": 3, "subject_name": "اللغة العربية", "teacher_name": null},
     {"day_of_week": "الأحد", "period_number": 4, "subject_name": "اللغة الإنكليزية", "teacher_name": null},
     {"day_of_week": "الأحد", "period_number": 5, "subject_name": "التربية الفنية", "teacher_name": null}
   ]}
   ```
   It replaces every period of that class; cells you leave out are removed. Other classes never
   change. The response has the same shape as the `GET`, with the schedule as now stored (each
   period with its `subject_key`); show it instead of your local copy. `subject_key` is
   read-only: you may send the periods back with it, and it's ignored.

**Rules for `PUT`:**

- **Empty save:** `"periods": []` clears that class's schedule. Ask the admin to confirm first.
  Leaving `periods` out (or `null`) is a 400, so a bug can't clear a class.
- **Day names:** send the Arabic names (with or without hamza) or the English names in any case.
  The server stores and returns `الأحد`, `الإثنين`, `الثلاثاء`, `الأربعاء`, `الخميس`. Friday,
  Saturday or anything else is 400.
- **Limits:**
  - `period_number` is 1 to 6, and each day follows the 5-or-6 rule.
  - `subject_name` is required, 1 to 100 characters.
  - `teacher_name` is optional, up to 100 characters; `null` or blank is saved as `null`.
  - `grade` and `section` are required, 1 to 50 characters, and saved with surrounding spaces
    removed.
  - At most 30 periods per request (5 days × 6), and each day and period pair at most once.
- **Errors:** a 400 `message` names the problem, the period by its 0-based index and the day, for
  example `periods[5].day_of_week الإثنين has periods 1, 2, 3, 4; a school day must have exactly
  5 or 6 periods, numbered 1 to 5 or 1 to 6`. A failed save (400 or 500) changes nothing.
- **Two admins saving the same class at once** (or a save during an Excel import): one wins
  entirely; the schedule is never a mix.
- **What parents see:** `GET /api/mobile/schedule` shows these periods to every active child whose
  `grade` and `section` are equal to the saved ones, in the same order. A missing teacher appears
  there as `""` (here it's `null`).
- **Schedules saved before API 1.13.0** may break the rule (4 periods, period 8, a Friday row, an
  English day name). They stay readable everywhere, unchanged, until the class is saved again;
  then the new save must follow the rule. Show such a class as it is and let the admin fix it.

**Excel flow (one class per file).** Show each class from `GET /api/admin/schedule/classes`
(for example "G1 / A") with three buttons. Send `grade` and `section` **exactly as the classes
endpoint returns them** (they're case-sensitive and stored as written):

- **Export:** `GET /api/admin/schedule/export?grade=G1&section=A` with the `Authorization`
  header. Save the response bytes as a file (a binary `.xlsx`, not JSON). Take the file name from
  `Content-Disposition` (`schedule_<grade>_<section>_<date>.xlsx`; an Arabic name comes only as
  `filename*=utf-8''…`, so use a parser that decodes it) or build the same name locally.
  - The file holds that class only: right-to-left, the ministry line, school name and the title
    with the class in rows 1 to 3, the column headers in row 5, and 30 rows from row 6 (Sunday to
    Thursday × periods 1 to 6) with the stored subject and teacher or blank cells. Every cell is
    text.
  - A class with students but no schedule downloads as a **blank template** to fill in.
  - 400 when `grade` or `section` is missing, blank or over 50 characters; **404** when no active
    student and no stored period has this class (refresh the class list).
- **Import:** let the admin pick an `.xlsx` and upload it as `multipart/form-data` to
  `POST /api/admin/schedule/import` with parts `file`, **`grade` and `section`** (the class of the
  button), and **`dry_run=true` first**. Show the result, and only after the admin confirms send
  the same file again without `dry_run` (or with `dry_run=false`).
  - **200:** `classes` has one item (`periods`, `matched_students`), plus `total_periods` and
    `warnings`. Show every warning: a class no active student has is saved, but no parent sees
    it.
  - **400 with `errors`:** a list of problems, each with `sheet`, `row` (the row number Excel
    shows), `column` (the standard Arabic header name) and `message`. Show them as a table so the
    admin can fix the file. **A file that contains a row of another class is rejected**: that row
    is listed (`this file is for grade G1, section A only`). At most 50 problems are listed, then
    one item with only a `message` such as `and 12 more problems`. Nothing was saved.
  - **400 without `errors`:** the file or request as a whole is wrong: not an `.xlsx`, empty, no
    header row, no filled row of this class (`The file has no rows for grade G1, section A`), only
    one of `grade`/`section`, or too many rows. Show `message`.
  - **413:** the file is over 2 MiB.
- **Clear:** `PUT /api/admin/schedule` with `"periods": []`, after a confirmation dialog. Importing
  can't clear a class (a file whose rows are all blank is refused).

Without the `grade` and `section` parts the import still accepts a file with several classes and
replaces each of them; the dashboard's per-class Import should always send them.

**Columns** (found by header name, in any order; extra columns are ignored; the header row must
be one of the first 10 rows, and title rows above it are fine):

| Column | Also accepted | Content |
|---|---|---|
| `المرحلة` | `الصف`, `grade` | Required, 1 to 50 characters, exactly as the students have it |
| `الشعبة` | `section` | Required, 1 to 50 characters, exactly as the students have it |
| `اليوم` | `day` | Sunday to Thursday, in Arabic (with or without hamza) or English |
| `الحصة` | `period` | A whole number 1 to 6 (Arabic-Indic or Persian digits are fine) |
| `المادة` | `subject` | Required when the row has a teacher, 1 to 100 characters |
| `المعلم` | `المدرس`, `teacher` | Optional, up to 100 characters |

**Import rules:**

- **Blank rows are ignored:** a row whose subject and teacher are both empty doesn't count, so
  the empty grid rows of the export are fine. Leave a sixth period empty for a 5-period day, and
  all six empty for a day without lessons.
- **The class is fully replaced**, exactly as with `PUT`, including days that have no filled row
  in the file. Other classes are untouched. To clear a class, use `PUT` with `"periods": []`.
- **All or nothing:** any problem anywhere saves nothing.
- Every worksheet with the header row is read (others, such as notes, are ignored). A day and
  period may appear once across all sheets.
- Grade and section are kept exactly as written (only surrounding spaces are removed), so `G3` and
  `g3` are different classes. Don't merge cells in the table: a merged cell is read as blank in
  every row but the first.
- Formulas are not evaluated; the value Excel saved is used. A formula with no saved value is an
  error naming the cell.
- Rows saved before API 1.13.0 on Friday or Saturday, or with a period above 6, aren't in the
  export; importing that class removes them.

### Announcements and notifications (الإعلانات والإشعارات)

The dashboard section the school names "الإعلانات والإشعارات" holds two things: the existing
banners (pictures in the parent carousel; their endpoints and paths are unchanged, see
[Banners](#banners)) and the new text announcements below. The new name is a label only.

**Compose form:**

- **Title** (required, up to 100 characters) and **text** (required, up to 500 characters).
  Surrounding spaces are removed.
- **Audience** selector, sent as `audience`:

  | Choice | `audience` | Who receives it |
  |---|---|---|
  | All parents | `{"type": "all"}` | Every parent in the system, **including parents whose children are all deactivated** |
  | One parent | `{"type": "parent", "parent_phone": "+9647000000101"}` | That parent. Pick from the students list (`GET /api/admin/students` has each student's `parent_phone`); any format parent login accepts works |
  | A grade | `{"type": "class", "grade": "G3"}` | Parents of active students in every section of G3 |
  | A section | `{"type": "class", "section": "A"}` | Parents of active students in section A of every grade |
  | A class | `{"type": "class", "grade": "G3", "section": "A"}` | Parents of active students in G3 / A |

  Grade and section match exactly (case-sensitive), as the weekly schedule does; take them from
  `GET /api/admin/schedule/classes`. A parent with two children in the audience gets one
  notification. Send only the fields of the chosen type: an extra field (even blank) is a 400,
  so a form bug can never turn "one class" into "everyone".

**Send in two steps:**

1. `POST /api/admin/announcements` with `"dry_run": true`. Nothing is written or sent; the answer
   is `{"data": {"id": null, "recipient_count": 24, "dry_run": true}}`.
2. Show a confirmation dialog: "send to 24 parents? It cannot be recalled." On confirm, send the
   same body without `dry_run`. The answer has the new `id` and the final `recipient_count`.

**Errors:** 400 names the problem (including `No parents match this audience`); 404 means no
parent has that phone number; **409** means the same title, text and audience was sent less than
a minute ago (usually a double tap): show "already sent" and don't retry. **An announcement
can't be edited, recalled or deleted after it is sent**; there are no such endpoints.

**Sent log:** `GET /api/admin/announcements` lists every announcement, newest first, 50 per page,
with the same `before` / `has_more` / `next_before` paging as the parent notifications list. Each
item has `title`, `body`, `audience` (`type`, `grade`, `section`, `parent_name`, `parent_phone`;
unused fields are `null`), `recipient_count`, `read_count` (how many parents have marked it read
so far) and `created_at`. Show "read by 17 of 24".

**What parents see:** the announcement arrives as an ordinary notification in
`GET /api/mobile/notifications`, unread, and raises `unread_count` like any other; nothing changes
in the parent app's endpoints. Each of the parent's phones also gets a push, sent in the
background after the announcement is saved. Push only arrives when the app has Firebase connected
and registered its token (see [Push notifications (FCM)](#push-notifications-fcm)), so test it on
a real phone; the notification list works without it.

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

**In API 1.14.0 (this release):** new admin endpoints `POST /api/admin/announcements` (send an
announcement to all parents, one parent, a grade, a section or a class, with a `dry_run`
preview) and `GET /api/admin/announcements` (the sent log with read counts). See
[Announcements and notifications](#announcements-and-notifications-الإعلانات-والإشعارات). Nothing
breaks: no existing endpoint or response changed.

**In API 1.13.1:** the schedule Excel export is one class per file
(`grade` and `section` are required; 404 for an unknown class), and the import takes optional
`grade` and `section` parts that restrict the file to that class. Breaking only for a client
that called the export without them.

**In API 1.13.0:**

- **Breaking for a dashboard that saves other period counts:** `PUT /api/admin/schedule` accepts
  only days with exactly 5 or 6 periods numbered from 1 (`period_number` 1 to 6, at most 30
  periods). Schedules saved earlier stay readable until saved again.
- **New:** `GET /api/admin/schedule/classes` (class overview), `GET /api/admin/schedule/export`
  (Excel download) and `POST /api/admin/schedule/import` (Excel upload; its 400 can carry an
  `errors` list, the one extension of the error envelope). See [Weekly schedule](#weekly-schedule).
- **Added field:** `subject_key` on every item of `GET /api/mobile/schedule` and of the admin
  schedule `GET` and `PUT` responses. Nothing else in those responses changed. See
  [Weekly schedule in the parent app](#weekly-schedule-in-the-parent-app).

**In API 1.12.0:**

- **`parent_pin` must be 16 to 72 printable ASCII characters with at least one letter, one digit
  and one symbol** whenever the admin sets or replaces it (400 otherwise). This breaks only a
  dashboard that still sends 6-digit values: generate the new format (see the admin dashboard
  notes). Field names are unchanged.
- **Parent login is unchanged:** it accepts any stored credential, including older 4- and 6-digit
  ones. Make the login field a normal text field, not a numeric keypad.

**In API 1.11.0:**

- **A student is counted from the day they were added.** The monthly records and summary no
  longer contain or count days before that; a month entirely before it leaves the child out.
  Past-date admin reports and Excel exports don't list students added after that date. Before,
  every school day of the month counted as `Absent`.
- **Today before 09:31 Asia/Baghdad:** the monthly records and summary leave out an `Absent`
  today until the check-in window closes. The today screen, the admin report and the dashboard
  are unchanged; show "not arrived yet" there (see Dates, times and time zone).
- Response shapes are unchanged.

**In API 1.10.0:**

- **New:** `DELETE /api/admin/leaves?student_id=&date=` cancels a leave (always 200; see the
  admin dashboard notes). Nothing existing changed.

**In API 1.9.0:**

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