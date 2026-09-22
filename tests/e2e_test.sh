#!/bin/bash
# Future Kids — End-to-End System Verification
# Tests the full data flow: Hardware Push -> Database -> Admin & Mobile APIs
# Usage: TEST_DATABASE_URL=<dsn> ./tests/e2e_test.sh
set -euo pipefail

DB_URL="${TEST_DATABASE_URL:-postgresql://yisawi@localhost:5432/future_kids?sslmode=disable}"
API_URL="${API_URL:-http://localhost:8080}"

# ── Colors ────────────────────────────────────────────────────────────────────
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

# ── Helpers ───────────────────────────────────────────────────────────────────
pass() { echo -e "${GREEN}  ✓ $1${NC}"; assertions_passed=$((assertions_passed + 1)); }
fail() { echo -e "${RED}  ✗ $1${NC}"; exit 1; }
section() { echo -e "\n${YELLOW}━━━ $1 ━━━${NC}"; }

assert_http() {
    local label="$1" actual="$2" expected="${3:-200}"
    if [ "$actual" -eq "$expected" ]; then
        pass "$label → HTTP $actual"
    else
        fail "$label → Expected HTTP $expected, got HTTP $actual"
    fi
}

assert_eq() {
    local label="$1" actual="$2" expected="$3"
    if [ "$actual" = "$expected" ]; then
        pass "$label → $actual"
    else
        fail "$label → Expected '$expected', got '$actual'"
    fi
}

assert_gt() {
    local label="$1" actual="$2" min="$3"
    if [ "$actual" -gt "$min" ]; then
        pass "$label → $actual (> $min)"
    else
        fail "$label → Expected > $min, got $actual"
    fi
}

assert_lt() {
    local label="$1" actual="$2" max="$3"
    if [ "$actual" -lt "$max" ]; then
        pass "$label → $actual (< $max)"
    else
        fail "$label → Expected < $max, got $actual"
    fi
}

# ── Counters ──────────────────────────────────────────────────────────────────
assertions_passed=0
start_time=$(date +%s)

echo -e "${CYAN}"
echo "  ╔════════════════════════════════════════╗"
echo "  ║   Future Kids — E2E System Verifier   ║"
echo "  ╚════════════════════════════════════════╝"
echo -e "${NC}"

# ── Verify server is up ───────────────────────────────────────────────────────
section "Pre-flight"
health_status=$(curl -s -o /dev/null -w "%{http_code}" "$API_URL/health")
assert_http "Server health check" "$health_status"

# ── Truncate tables for a clean slate ────────────────────────────────────────
echo -e "\n${YELLOW}Resetting database...${NC}"
psql "$DB_URL" -q -c "TRUNCATE attendance_logs, student_leaves, students, parents, devices, notifications, settings CASCADE;"
pass "Database truncated"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 1 — Admin Authentication"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
admin_res=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/api/admin/login" \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}')
admin_http=$(echo "$admin_res" | tail -n1)
admin_body=$(echo "$admin_res" | sed '$d')
assert_http "Admin login" "$admin_http"

ADMIN_TOKEN=$(echo "$admin_body" | jq -r '.data.token')
[ "$ADMIN_TOKEN" != "null" ] && [ -n "$ADMIN_TOKEN" ] || fail "Admin JWT is null or empty"
pass "Admin JWT extracted"

assert_eq "Admin response status field" "$(echo "$admin_body" | jq -r '.status')" "success"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 2 — Admin Setup (Devices & Students)"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
for sn in "DEVICE-E2E-001" "DEVICE-E2E-002"; do
    d_http=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$API_URL/api/admin/devices" \
      -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
      -d "{\"serial_number\":\"$sn\",\"location_name\":\"Gate\",\"is_active\":true}")
    assert_http "Create device $sn" "$d_http"
done

# Create 10 students across 2 parents
PARENT_A="+9647700000101"
PARENT_B="+9647700000102"
declare -a RFID_TAGS

for i in {1..10}; do
    rfid="E2E-RFID-$(printf "%03d" "$i")"
    RFID_TAGS+=("$rfid")
    phone=$PARENT_A; [ $((i % 2)) -eq 0 ] && phone=$PARENT_B
    s_http=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$API_URL/api/admin/students" \
      -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
      -d "{\"name\":\"E2E Student $i\",\"parent_name\":\"E2E Parent\",\"parent_phone\":\"$phone\",\"parent_pin\":\"1234\",\"rfid_tag\":\"$rfid\"}")
    assert_http "Create student $i" "$s_http"
done

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 3 — Hardware ADMS Push (ZKTeco Behavioral Time-Window Test)"
# Simulates the full real-world multi-punch sequence for student[0] to validate
# time-window logic, dead-zone filtering, and idempotency per RULES.md Section 5.
#
# Punches sent for student 0 (RFID_TAGS[0]):
#   07:15 → valid check-in  (Morning window: 06:30–09:30) ← MUST be recorded
#   07:18 → spam  check-in  (still in Morning window)     ← MUST be ignored
#   10:30 → dead-zone punch (09:31–11:29)                 ← MUST be ignored
#   12:05 → valid check-out (Afternoon window: 11:30–13:30) ← MUST be recorded
#   12:15 → spam check-out  (still in Afternoon window)   ← MUST be ignored
#
# Students 1–4 get a simple valid check-in + check-out.
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
TODAY=$(date +"%Y-%m-%d")

# Student 0: Full behavioral sequence (time-window + dead-zone + spam proof)
BEHAVIORAL_RFID="${RFID_TAGS[0]}"
ATTLOG_BODY=""
ATTLOG_BODY+="${BEHAVIORAL_RFID}\t${TODAY} 07:15:00\t1\t1\n"  # valid check-in
ATTLOG_BODY+="${BEHAVIORAL_RFID}\t${TODAY} 07:18:00\t1\t1\n"  # spam — must be ignored
ATTLOG_BODY+="${BEHAVIORAL_RFID}\t${TODAY} 10:30:00\t1\t1\n"  # dead-zone — must be ignored
ATTLOG_BODY+="${BEHAVIORAL_RFID}\t${TODAY} 12:05:00\t1\t1\n"  # valid check-out
ATTLOG_BODY+="${BEHAVIORAL_RFID}\t${TODAY} 12:15:00\t1\t1\n"  # spam — must be ignored

# Students 1–4: simple valid check-in + check-out
for i in {1..4}; do
    rfid="${RFID_TAGS[$i]}"
    ATTLOG_BODY+="${rfid}\t${TODAY} 07:$(printf "%02d" $((i * 3))):00\t1\t1\n"
    ATTLOG_BODY+="${rfid}\t${TODAY} 12:$(printf "%02d" $((i * 3))):00\t1\t1\n"
done

ATTLOG_PAYLOAD=$(printf "%b" "$ATTLOG_BODY")

adms_res=$(curl -s -w "\n%{http_code}" -X POST \
  "${API_URL}/iclock/cdata?SN=DEVICE-E2E-001&table=ATTLOG" \
  -H "Content-Type: text/plain" \
  --data-binary "$ATTLOG_PAYLOAD")
adms_http=$(echo "$adms_res" | tail -n1)
adms_body=$(echo "$adms_res" | sed '$d')
assert_http "ADMS ATTLOG push" "$adms_http"
assert_eq  "ADMS response body is plain OK" "$adms_body" "OK"

# Idempotency check: re-push the SAME payload — device must still get HTTP 200 OK
dup_http=$(curl -s -o /dev/null -w "%{http_code}" -X POST \
  "${API_URL}/iclock/cdata?SN=DEVICE-E2E-001&table=ATTLOG" \
  -H "Content-Type: text/plain" \
  --data-binary "$ATTLOG_PAYLOAD")
assert_http "ADMS duplicate push (idempotency)" "$dup_http"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 4 — Admin Daily Attendance (Strict Time-Window Behavioral Validation)"
# Asserts that the API returns EXACT times for student[0], not the spam/dead-zone
# punches. This is the behavioral proof of RULES.md Section 5.
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
att_res=$(curl -s -w "\n%{http_code}" -X GET \
  "${API_URL}/api/admin/attendance?date=${TODAY}" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
att_http=$(echo "$att_res" | tail -n1)
att_body=$(echo "$att_res" | sed '$d')
assert_http "Admin daily attendance" "$att_http"
assert_eq  "Admin attendance status field" "$(echo "$att_body" | jq -r '.status')" "success"
assert_eq  "Admin attendance date field"   "$(echo "$att_body" | jq -r '.date')"   "$TODAY"

# Find the behavioral student (student[0] = RFID_TAGS[0]) by rfid in the name match
# The API returns students ordered by status desc, name asc — find student[0]'s record.
behavioral_record=$(echo "$att_body" | jq --arg name "E2E Student 1" '.data[] | select(.full_name == $name)')
[ -n "$behavioral_record" ] && [ "$behavioral_record" != "null" ] || fail "Behavioral student record not found in Admin attendance"

# Assert status is Present (had a valid morning punch)
assert_eq "Admin: behavioral student is Present" \
    "$(echo "$behavioral_record" | jq -r '.status')" "Present"

# STRICT: check_in_time must be exactly 07:15 AM — the FIRST valid punch.
# If the system returned 07:18 AM (spam) the time-window logic is broken.
assert_eq "Admin: check_in_time is 07:15 AM (not spam 07:18 AM)" \
    "$(echo "$behavioral_record" | jq -r '.check_in_time')" "07:15 AM"

# STRICT: check_out_time must be exactly 12:05 PM — the FIRST valid afternoon punch.
# If the system returned 12:15 PM (spam) or 10:30 AM (dead-zone) the logic is broken.
assert_eq "Admin: check_out_time is 12:05 PM (not spam 12:15 PM or dead-zone 10:30 AM)" \
    "$(echo "$behavioral_record" | jq -r '.check_out_time')" "12:05 PM"

# At least 5 students should now be 'Present' (we punched 5)
present_count=$(echo "$att_body" | jq '[.data[] | select(.status == "Present")] | length')
assert_eq "Present count after 5 ADMS punches" "$present_count" "5"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 5 — Admin Dashboard Stats"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
dash_res=$(curl -s -w "\n%{http_code}" -X GET "$API_URL/api/admin/dashboard" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
dash_http=$(echo "$dash_res" | tail -n1)
dash_body=$(echo "$dash_res" | sed '$d')
assert_http "Admin dashboard" "$dash_http"
assert_eq  "Dashboard status field"         "$(echo "$dash_body" | jq -r '.status')"              "success"
assert_eq  "Dashboard total_students = 10"  "$(echo "$dash_body" | jq -r '.data.total_students')" "10"
assert_eq  "Dashboard present_today = 5"    "$(echo "$dash_body" | jq -r '.data.present_today')"  "5"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 6 — Admin Excel Export"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
excel_res=$(curl -s -I -X GET "${API_URL}/api/admin/export/excel?date=${TODAY}" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
excel_http=$(echo "$excel_res" | head -n1 | awk '{print $2}')
excel_ct=$(echo "$excel_res" | grep -i "Content-Type:" | awk '{print $2}' | tr -d '\r')
assert_http "Excel export HTTP"         "$excel_http"
assert_eq  "Excel Content-Type header" "$excel_ct" \
    "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 7 — Mobile Authentication"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
mob_login_res=$(curl -s -w "\n%{http_code}" -X POST "$API_URL/api/mobile/login" \
  -H "Content-Type: application/json" \
  -d "{\"phone\":\"$PARENT_A\",\"pin\":\"1234\"}")
mob_login_http=$(echo "$mob_login_res" | tail -n1)
mob_login_body=$(echo "$mob_login_res" | sed '$d')
assert_http "Mobile login" "$mob_login_http"
assert_eq  "Mobile login status field"  "$(echo "$mob_login_body" | jq -r '.status')"       "success"
assert_eq  "Mobile login parent phone"  "$(echo "$mob_login_body" | jq -r '.data.parent.phone')" "$PARENT_A"

PARENT_TOKEN=$(echo "$mob_login_body" | jq -r '.data.token')
[ "$PARENT_TOKEN" != "null" ] && [ -n "$PARENT_TOKEN" ] || fail "Mobile JWT is null or empty"
pass "Mobile JWT extracted"

# Verify wrong PIN is rejected
wrong_pin_http=$(curl -s -o /dev/null -w "%{http_code}" -X POST "$API_URL/api/mobile/login" \
  -H "Content-Type: application/json" \
  -d "{\"phone\":\"$PARENT_A\",\"pin\":\"WRONG\"}")
assert_http "Wrong PIN rejected" "$wrong_pin_http" "401"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 8 — Mobile Today Attendance (Strict Time-Window Behavioral Validation)"
# Mirrors Phase 4 from the parent's perspective — asserting exact time values
# from the same behavioral student[0] punch sequence.
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
mob_today_res=$(curl -s -w "\n%{http_code}" -X GET "$API_URL/api/mobile/attendance/today" \
  -H "Authorization: Bearer $PARENT_TOKEN")
mob_today_http=$(echo "$mob_today_res" | tail -n1)
mob_today_body=$(echo "$mob_today_res" | sed '$d')
assert_http "Mobile today attendance" "$mob_today_http"
assert_eq  "Mobile attendance status field" "$(echo "$mob_today_body" | jq -r '.status')" "success"
assert_eq  "Mobile attendance date field"   "$(echo "$mob_today_body" | jq -r '.date')"   "$TODAY"

# Parent A is the parent of students 1,3,5,7,9 (odd-indexed in setup loop)
# Find the behavioral student[0] record ("E2E Student 1") in the parent's view
mob_behavioral=$(echo "$mob_today_body" | jq --arg name "E2E Student 1" '.data[] | select(.full_name == $name)')
[ -n "$mob_behavioral" ] && [ "$mob_behavioral" != "null" ] || fail "Behavioral student not found in Mobile attendance"

# Assert status is Present
assert_eq "Mobile: behavioral student is Present" \
    "$(echo "$mob_behavioral" | jq -r '.status')" "Present"

# STRICT: check_in_time must be exactly 07:15 AM — spam 07:18 AM must NOT appear
assert_eq "Mobile: check_in_time is 07:15 AM (not spam 07:18 AM)" \
    "$(echo "$mob_behavioral" | jq -r '.check_in_time')" "07:15 AM"

# STRICT: check_out_time must be exactly 12:05 PM — spam 12:15 PM must NOT appear
assert_eq "Mobile: check_out_time is 12:05 PM (not spam 12:15 PM or dead-zone 10:30 AM)" \
    "$(echo "$mob_behavioral" | jq -r '.check_out_time')" "12:05 PM"

# Parent A should see only their own children (5 out of 10), not all 10
mob_count=$(echo "$mob_today_body" | jq '.data | length')
assert_eq  "Parent A data isolation — sees 5 children" "$mob_count" "5"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 9 — Security: Unauthenticated Access Rejected"
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
no_auth_http=$(curl -s -o /dev/null -w "%{http_code}" -X GET "$API_URL/api/admin/dashboard")
assert_http "No-token admin dashboard rejected" "$no_auth_http" "401"

bad_token_http=$(curl -s -o /dev/null -w "%{http_code}" -X GET "$API_URL/api/mobile/attendance/today" \
  -H "Authorization: Bearer totally.invalid.token")
assert_http "Bad token rejected"                "$bad_token_http" "401"

wrong_role_http=$(curl -s -o /dev/null -w "%{http_code}" -X GET "$API_URL/api/admin/dashboard" \
  -H "Authorization: Bearer $PARENT_TOKEN")
assert_http "Parent token rejected on admin route" "$wrong_role_http" "403"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
section "Phase 10 — respondError JSON Shape Validation"
# Ensures our centralized respondError helper always returns valid, parseable JSON
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
err_body=$(curl -s -X GET "$API_URL/api/admin/dashboard")  # no token
err_status=$(echo "$err_body" | jq -r '.status'   2>/dev/null || echo "PARSE_FAIL")
err_msg=$(echo "$err_body"    | jq -r '.message'  2>/dev/null || echo "PARSE_FAIL")
assert_eq  "respondError .status field"  "$err_status" "error"
[ "$err_msg" != "PARSE_FAIL" ] && [ -n "$err_msg" ] || fail "respondError .message missing or not JSON"
pass "respondError .message present: $err_msg"

# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
# Final Report
# ━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
end_time=$(date +%s)
total_time=$((end_time - start_time))

echo -e "\n${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "${CYAN}  E2E REPORT${NC}"
echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
echo -e "  Total execution time : ${total_time}s"
echo -e "  Assertions passed    : ${GREEN}${assertions_passed}${NC}"
echo -e "${GREEN}\n  ✓ ALL SYSTEMS GO — E2E PASSED${NC}"
echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}\n"
