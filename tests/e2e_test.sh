#!/bin/bash
set -e

DB_URL="${TEST_DATABASE_URL:-postgresql://yisawi@localhost:5432/future_kids?sslmode=disable}"
API_URL="http://localhost:8080"

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${YELLOW}Starting Future Kids E2E Automation Test...${NC}"

# Truncate tables
echo -e "\n${YELLOW}--- Truncating Database ---${NC}"
psql "$DB_URL" -c "TRUNCATE attendance_logs, student_leaves, students, parents, devices, notifications, settings CASCADE;"
echo -e "${GREEN}Database truncated successfully.${NC}"

start_time=$(date +%s)
assertions_passed=0
total_endpoints=0
failed_requests=0

function track_endpoint() {
    total_endpoints=$((total_endpoints + 1))
}

function check_success() {
    if [ "$1" -eq 200 ] || [ "$1" -eq 201 ]; then
        echo -e "${GREEN}SUCCESS (HTTP $1)${NC}"
    else
        echo -e "${RED}FAILED (HTTP $1)${NC}"
        failed_requests=$((failed_requests + 1))
        exit 1
    fi
}

# --- Phase 1: Admin Setup ---
echo -e "\n${YELLOW}--- Phase 1: Admin Setup ---${NC}"

track_endpoint
echo "Authenticating as Admin..."
admin_res=$(curl -s -w "\n%{http_code}" -X POST $API_URL/api/admin/login \
  -H "Content-Type: application/json" \
  -d '{"username":"admin","password":"admin123"}')
admin_status=$(echo "$admin_res" | tail -n1)
admin_body=$(echo "$admin_res" | sed '$d')
check_success "$admin_status"
ADMIN_TOKEN=$(echo "$admin_body" | jq -r .data.token)

track_endpoint
echo "Creating 2 Hardware Devices..."
device_1_res=$(curl -s -w "\n%{http_code}" -X POST $API_URL/api/admin/devices \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"serial_number": "DEVICE-001", "location_name": "Gate 1", "is_active": true}')
check_success "$(echo "$device_1_res" | tail -n1)"

device_2_res=$(curl -s -w "\n%{http_code}" -X POST $API_URL/api/admin/devices \
  -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"serial_number": "DEVICE-002", "location_name": "Gate 2", "is_active": true}')
check_success "$(echo "$device_2_res" | tail -n1)"

track_endpoint
echo "Creating 150 Students (assigned randomly to 3 parents)..."
parents_phones=("+9647700000001" "+9647700000002" "+9647700000003")
declare -a rfid_tags

for i in {1..150}; do
    parent_index=$((RANDOM % 3))
    parent_phone=${parents_phones[$parent_index]}
    rfid="RFID-$(printf "%04d" $i)"
    rfid_tags+=("$rfid")

    res=$(curl -s -o /dev/null -w "%{http_code}" -X POST $API_URL/api/admin/students \
      -H "Authorization: Bearer $ADMIN_TOKEN" -H "Content-Type: application/json" \
      -d '{"name": "Student '$i'", "parent_name": "Parent '$parent_index'", "parent_phone": "'$parent_phone'", "parent_pin": "1234", "rfid_tag": "'$rfid'"}')
    
    if [ "$res" -ne 200 ]; then
        echo -e "${RED}Failed to create student $i (HTTP $res)${NC}"
        exit 1
    fi
done
echo -e "${GREEN}Successfully created 150 students and 3 parents.${NC}"


# --- Phase 2: Hardware Stress Test ---
echo -e "\n${YELLOW}--- Phase 2: Hardware Stress Test (Concurrency) ---${NC}"
track_endpoint

# Generate curl commands
rm -f punch_results.txt
for rfid in "${rfid_tags[@]}"; do
    device="DEVICE-001"
    if [ $((RANDOM % 2)) -eq 0 ]; then device="DEVICE-002"; fi
    
    today=$(date +"%Y-%m-%d")
    hour=$((RANDOM % 2 + 7)) # 7 or 8 AM
    minute=$(printf "%02d" $((RANDOM % 60)))
    second=$(printf "%02d" $((RANDOM % 60)))
    push_time="$today 0$hour:$minute:$second"
    
    payload="{\"device_sn\": \"$device\", \"rfid_tag\": \"$rfid\", \"push_time\": \"$push_time\"}"
    
    curl -s -w "%{http_code}\n" -o /dev/null -X POST "$API_URL/api/attendance/push/json?SN=$device" -H "Content-Type: application/json" -d "$payload" >> punch_results.txt &
    
    # Introduce duplicates (30% chance) to test idempotency
    if [ $((RANDOM % 100)) -lt 30 ]; then
        curl -s -w "%{http_code}\n" -o /dev/null -X POST "$API_URL/api/attendance/push/json?SN=$device" -H "Content-Type: application/json" -d "$payload" >> punch_results.txt &
    fi
done

total_punches=$(wc -l < punch_results.txt 2>/dev/null || echo 0) # will be 0 before wait, just indicative
echo "Sending punches (including duplicates) concurrently..."

stress_start=$(date +%s%N)
wait
stress_end=$(date +%s%N)
stress_duration_ms=$(( (stress_end - stress_start) / 1000000 ))

total_punches=$(wc -l < punch_results.txt | awk '{print $1}')

# Check for failures (Any status other than 200 OK)
failures=$(grep -v "200" punch_results.txt | wc -l | awk '{print $1}')
if [ "$failures" -gt 0 ]; then
    echo -e "${RED}Hardware stress test failed: $failures requests did not return 200 OK.${NC}"
    exit 1
else
    echo -e "${GREEN}All $total_punches punches processed successfully (200 OK) in ${stress_duration_ms}ms.${NC}"
fi


# --- Phase 3: Admin Operations & Validation ---
echo -e "\n${YELLOW}--- Phase 3: Admin Operations & Validation ---${NC}"

echo "Updating a student's grade/section via direct DB execution..."
psql "$DB_URL" -c "UPDATE students SET grade='Grade 1', section='A' WHERE id = (SELECT id FROM students LIMIT 1);" > /dev/null
echo -e "${GREEN}Student updated successfully.${NC}"

track_endpoint
echo "Soft deleting a device (DEVICE-002)..."
del_res=$(curl -s -w "\n%{http_code}" -X DELETE "$API_URL/api/admin/devices?sn=DEVICE-002" \
  -H "Authorization: Bearer $ADMIN_TOKEN")
check_success "$(echo "$del_res" | tail -n1)"

track_endpoint
echo "Fetching dashboard metrics..."
dash_res=$(curl -s -X GET $API_URL/api/admin/dashboard \
  -H "Authorization: Bearer $ADMIN_TOKEN")
  
total_students=$(echo "$dash_res" | jq .data.total_students)
present_today=$(echo "$dash_res" | jq .data.present_today)

if [ "$total_students" -eq 150 ]; then
    echo -e "${GREEN}Assertion Passed: Total students = 150${NC}"
    assertions_passed=$((assertions_passed + 1))
else
    echo -e "${RED}Assertion Failed: Total students = $total_students (Expected: 150)${NC}"
    failed_requests=$((failed_requests + 1))
fi

if [ "$present_today" -eq 150 ]; then
    echo -e "${GREEN}Assertion Passed: Present today = 150 (all duplicates were correctly ignored/silenced)${NC}"
    assertions_passed=$((assertions_passed + 1))
else
    echo -e "${RED}Assertion Failed: Present today = $present_today (Expected: 150)${NC}"
    failed_requests=$((failed_requests + 1))
fi

track_endpoint
echo "Exporting Excel report..."
excel_res=$(curl -s -I -X GET $API_URL/api/admin/export/excel \
  -H "Authorization: Bearer $ADMIN_TOKEN")

excel_status=$(echo "$excel_res" | head -n 1 | awk '{print $2}')
excel_type=$(echo "$excel_res" | grep -i "Content-Type:" | awk '{print $2}' | tr -d '\r')

if [ "$excel_status" == "200" ] && [ "$excel_type" == "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet" ]; then
    echo -e "${GREEN}Assertion Passed: Excel exported successfully with correct headers.${NC}"
    assertions_passed=$((assertions_passed + 1))
else
    echo -e "${RED}Assertion Failed: Excel export returned Status $excel_status, Type $excel_type${NC}"
    failed_requests=$((failed_requests + 1))
fi


# --- Phase 4: Mobile / Parent View ---
echo -e "\n${YELLOW}--- Phase 4: Mobile / Parent View ---${NC}"

track_endpoint
echo "Authenticating as Parent 1..."
parent_res=$(curl -s -w "\n%{http_code}" -X POST $API_URL/api/mobile/login \
  -H "Content-Type: application/json" \
  -d '{"phone":"+9647700000001","pin":"1234"}')
parent_status=$(echo "$parent_res" | tail -n1)
parent_body=$(echo "$parent_res" | sed '$d')
check_success "$parent_status"
PARENT_TOKEN=$(echo "$parent_body" | jq -r .data.token)

track_endpoint
echo "Fetching mobile attendance summary..."
mob_res=$(curl -s -X GET $API_URL/api/mobile/attendance/summary \
  -H "Authorization: Bearer $PARENT_TOKEN")

children_count=$(echo "$mob_res" | jq '.data | length')
if [ "$children_count" -gt 0 ] && [ "$children_count" -lt 150 ]; then
    echo -e "${GREEN}Assertion Passed: Parent only sees isolated data (Found $children_count children instead of 150)${NC}"
    assertions_passed=$((assertions_passed + 1))
else
    echo -e "${RED}Assertion Failed: Parent data isolation error! Count: $children_count${NC}"
    failed_requests=$((failed_requests + 1))
fi


# --- Phase 5: Performance & Health Report ---
end_time=$(date +%s)
total_time=$((end_time - start_time))

echo -e "\n${YELLOW}=========================================${NC}"
echo -e "${YELLOW}   E2E PERFORMANCE & HEALTH REPORT       ${NC}"
echo -e "${YELLOW}=========================================${NC}"
echo -e "Total Execution Time:    ${total_time} seconds"
echo -e "Hardware Stress Avg Time:${stress_duration_ms} ms (for $total_punches requests)"
echo -e "Total Endpoints Tested:  $total_endpoints"
echo -e "Successful Assertions:   ${GREEN}$assertions_passed${NC}"
if [ "$failed_requests" -eq 0 ]; then
    echo -e "Failed Requests:         ${GREEN}0${NC}"
    echo -e "\n${GREEN}STATUS: ALL SYSTEMS GO (HEALTHY)${NC}"
else
    echo -e "Failed Requests:         ${RED}$failed_requests${NC}"
    echo -e "\n${RED}STATUS: UNHEALTHY (ERRORS DETECTED)${NC}"
fi
echo -e "${YELLOW}=========================================${NC}"
