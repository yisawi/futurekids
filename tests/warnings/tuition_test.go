package warnings

import (
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"future_kids/internal/handlers"
)

const (
	tuiPath     = "/api/admin/tuition"
	tuiFeesPath = "/api/admin/tuition/fees"
	tuiDiscPath = "/api/admin/tuition/discount"
	tuiPayPath  = "/api/admin/tuition/payments"
	tuiVoidPath = "/api/admin/tuition/payments/void"
	tuiYear     = "2026-2027"
	tuiNextYear = "2027-2028"
	tuiG1       = "الصف الأول الابتدائي"
	tuiG2       = "الصف الثاني الابتدائي"
	tuiG3       = "الصف الثالث الابتدائي"
)

// tuiStudent inserts an active student directly and returns its id.
func tuiStudent(t *testing.T, db *sql.DB, name, grade, section string) int {
	t.Helper()
	var id int
	if err := db.QueryRow(`
		INSERT INTO students (full_name, rfid_tag, grade, section)
		VALUES ($1, (SELECT (COALESCE(MAX(id), 0) + 5000)::text FROM students), NULLIF($2, ''), NULLIF($3, ''))
		RETURNING id`, name, grade, section).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// tuiV renders a JSON value for comparisons: numbers as integers, null as "null".
func tuiV(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case float64:
		return strconv.FormatInt(int64(x), 10)
	}
	return fmt.Sprint(v)
}

// tuiFigures renders a tuition row or detail as "grade fee discount paid remaining fee_set".
func tuiFigures(d map[string]any) string {
	return fmt.Sprintf("%s fee=%s discount=%s paid=%s remaining=%s set=%s",
		tuiV(d["grade"]), tuiV(d["fee"]), tuiV(d["discount"]), tuiV(d["paid"]), tuiV(d["remaining"]), tuiV(d["fee_set"]))
}

func tuiBody(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

type tuiClient struct {
	t     *testing.T
	srv   *g3Server
	admin string
}

func (c tuiClient) do(method, path string, body any) a14Response {
	c.t.Helper()
	return a14Do(c.t, c.srv, method, path, l1Header(c.admin), body)
}

func (c tuiClient) ok(what, method, path string, body any) map[string]any {
	c.t.Helper()
	r := c.do(method, path, body)
	if r.status != 200 {
		c.t.Fatalf("%s: %d %s", what, r.status, r.body)
	}
	return r.json(c.t)
}

func (c tuiClient) fee(grade string, fee int64, apply bool) a14Response {
	c.t.Helper()
	return c.do("PUT", tuiFeesPath, tuiBody("grade", grade, "fee", fee, "apply_to_existing", apply))
}

func (c tuiClient) pay(student int, amount int64, kv ...any) a14Response {
	c.t.Helper()
	return c.do("POST", tuiPayPath, tuiBody(append([]any{"student_id", student, "amount", amount}, kv...)...))
}

func (c tuiClient) detail(student int, year string) map[string]any {
	c.t.Helper()
	path := fmt.Sprintf("%s/student?student_id=%d", tuiPath, student)
	if year != "" {
		path += "&academic_year=" + year
	}
	return c.ok("detail", "GET", path, nil)["data"].(map[string]any)
}

func (c tuiClient) figures(student int, year string) string {
	c.t.Helper()
	return tuiFigures(c.detail(student, year))
}

func (c tuiClient) setYear(year string) a14Response {
	c.t.Helper()
	return c.do("PUT", "/api/admin/settings", tuiBody("key", "academic_year", "value", year))
}

// list returns the rows (by student id) and the summary of GET /api/admin/tuition?query.
func (c tuiClient) list(query string) (map[int]map[string]any, []int, map[string]any) {
	c.t.Helper()
	m := c.ok("list "+query, "GET", tuiPath+"?"+query, nil)
	rows := map[int]map[string]any{}
	var ids []int
	for _, it := range m["data"].([]any) {
		row := it.(map[string]any)
		id := int(row["student_id"].(float64))
		rows[id] = row
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return rows, ids, m["summary"].(map[string]any)
}

func tuiPaymentID(t *testing.T, r a14Response) int64 {
	t.Helper()
	pays := r.json(t)["data"].(map[string]any)["payments"].([]any)
	return int64(pays[0].(map[string]any)["id"].(float64))
}

func tuiSummary(s map[string]any) string {
	return fmt.Sprintf("fee=%s discount=%s collected=%s remaining=%s not_set=%s",
		tuiV(s["total_fee"]), tuiV(s["total_discount"]), tuiV(s["total_collected"]), tuiV(s["total_remaining"]), tuiV(s["not_set_count"]))
}

func tuiWant(t *testing.T, what, got, want string) {
	t.Helper()
	if got != want {
		t.Errorf("%s:\n got  %s\n want %s", what, got, want)
	}
}

// TestTuitionValidation verifies the 400s (amounts, dates, lengths, academic years, ids, JSON),
// the 404s and the request limits, and that a refused request writes nothing.
func TestTuitionValidation(t *testing.T) {
	srv, db, admin := l1Server(t, "tuivalid", true)
	c := tuiClient{t, srv, admin}
	sid := tuiStudent(t, db, "طالب اختبار", tuiG1, "أ")
	if r := c.fee(tuiG1, 1000000, false); r.status != 200 {
		t.Fatalf("set fee: %d %s", r.status, r.body)
	}
	long := strings.Repeat("ب", 201)
	rangeMsg := func(field string, min int) string { return fmt.Sprintf("%s must be from %d to 1000000000", field, min) }
	for _, cs := range []struct {
		name, method, path string
		body               any
		status             int
		msg                string
	}{
		{"amount 0", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 0), 400, rangeMsg("amount", 1)},
		{"amount negative", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", -1), 400, rangeMsg("amount", 1)},
		{"amount huge", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 1000000001), 400, rangeMsg("amount", 1)},
		{"amount missing", "POST", tuiPayPath, tuiBody("student_id", sid), 400, "amount is required"},
		{"amount not an integer", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 1.5), 400, "Invalid request body"},
		{"amount a string", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", "100"), 400, "Invalid request body"},
		{"amount beyond int64", "POST", tuiPayPath, fmt.Sprintf(`{"student_id":%d,"amount":1e30}`, sid), 400, "Invalid request body"},
		{"student_id missing", "POST", tuiPayPath, tuiBody("amount", 100), 400, handlers.TuitionStudentIDMessage},
		{"paid_at tomorrow", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 100, "paid_at", "2026-09-25"), 400, handlers.TuitionPaidAtFuture},
		{"paid_at month 13", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 100, "paid_at", "2026-13-01"), 400, handlers.TuitionPaidAtMessage},
		{"paid_at day first", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 100, "paid_at", "24-09-2026"), 400, handlers.TuitionPaidAtMessage},
		{"note 201 characters", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 100, "note", long), 400, "note must be at most 200 characters"},
		{"payment academic_year", "POST", tuiPayPath, tuiBody("student_id", sid, "amount", 100, "academic_year", "2026-2028"), 400, handlers.AcademicYearMessage},
		{"malformed JSON", "POST", tuiPayPath, "{", 400, "Invalid request body"},
		{"unknown student", "POST", tuiPayPath, tuiBody("student_id", 999999, "amount", 100), 404, "Student not found"},
		{"fee grade missing", "PUT", tuiFeesPath, tuiBody("fee", 100), 400, "grade is required"},
		{"fee grade 51 characters", "PUT", tuiFeesPath, tuiBody("grade", strings.Repeat("ب", 51), "fee", 100), 400, "grade must be at most 50 characters"},
		{"fee 0", "PUT", tuiFeesPath, tuiBody("grade", tuiG1, "fee", 0), 400, rangeMsg("fee", 1)},
		{"fee missing", "PUT", tuiFeesPath, tuiBody("grade", tuiG1), 400, "fee is required"},
		{"fee not an integer", "PUT", tuiFeesPath, tuiBody("grade", tuiG1, "fee", 99.5), 400, "Invalid request body"},
		{"discount negative", "PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", -1), 400, rangeMsg("discount", 0)},
		{"discount missing", "PUT", tuiDiscPath, tuiBody("student_id", sid), 400, "discount is required"},
		{"discount note 201 characters", "PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 0, "note", long), 400, "note must be at most 200 characters"},
		{"discount academic_year", "PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 0, "academic_year", "2026/2027"), 400, handlers.AcademicYearMessage},
		{"void id missing", "POST", tuiVoidPath, tuiBody("reason", "x"), 400, handlers.TuitionPaymentIDMessage},
		{"void reason missing", "POST", tuiVoidPath, tuiBody("id", 1, "reason", "  "), 400, "reason is required"},
		{"void reason 201 characters", "POST", tuiVoidPath, tuiBody("id", 1, "reason", long), 400, "reason must be at most 200 characters"},
		{"void unknown payment", "POST", tuiVoidPath, tuiBody("id", 999999, "reason", "x"), 404, "Payment not found"},
		{"list academic_year", "GET", tuiPath + "?academic_year=26-27", nil, 400, handlers.AcademicYearMessage},
		{"list has_remaining", "GET", tuiPath + "?has_remaining=yes", nil, 400, handlers.TuitionHasRemainingError},
		{"detail student_id", "GET", tuiPath + "/student?student_id=abc", nil, 400, handlers.TuitionStudentIDMessage},
		{"detail unknown student", "GET", tuiPath + "/student?student_id=999999", nil, 404, "Student not found"},
		{"detail academic_year", "GET", fmt.Sprintf("%s/student?student_id=%d&academic_year=2027-2026", tuiPath, sid), nil, 400, handlers.AcademicYearMessage},
	} {
		tchExpect(t, cs.name, c.do(cs.method, cs.path, cs.body), cs.status, cs.msg)
	}
	for _, y := range []string{"2026-2028", "2026/2027", "26-27", "2027-2026", "", "2026-2027x"} {
		tchExpect(t, "setting academic_year "+y, c.setYear(y), 400, handlers.AcademicYearMessage)
	}
	pad := strings.Repeat("a", 70<<10)
	for _, p := range []string{tuiPayPath, tuiFeesPath, tuiDiscPath, tuiVoidPath} {
		method := "POST"
		if p == tuiFeesPath || p == tuiDiscPath {
			method = "PUT"
		}
		tchExpect(t, "oversized body "+p, c.do(method, p, `{"pad":"`+pad+`"}`), 413, "")
		tchExpect(t, "no token "+p, a14Do(t, srv, method, p, nil, tuiBody()), 401, "")
	}
	tchExpect(t, "no token list", a14Do(t, srv, "GET", tuiPath, nil, nil), 401, "")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_accounts`) + k1Count(t, db, `SELECT COUNT(*) FROM tuition_payments`); n != 0 {
		t.Fatalf("refused requests wrote %d tuition rows", n)
	}
	if y := k1Count(t, db, `SELECT COUNT(*) FROM settings WHERE setting_key = 'academic_year' AND setting_value = $1`, tuiYear); y != 1 {
		t.Fatalf("refused year changes altered the setting")
	}

	// The limits themselves are accepted: 1,000,000,000 and a past or default (today) paid_at.
	big := tuiStudent(t, db, "طالب الحد الأعلى", tuiG2, "أ")
	c.fee(tuiG2, 1000000000, false)
	r := c.pay(big, 1000000000)
	tchExpect(t, "amount 1,000,000,000", r, 200, "Payment recorded")
	d := r.json(t)["data"].(map[string]any)
	tuiWant(t, "after the maximum payment", tuiFigures(d), tuiG2+" fee=1000000000 discount=0 paid=1000000000 remaining=0 set=true")
	if p := d["payments"].([]any)[0].(map[string]any); p["paid_at"] != k1Today || p["status"] != "active" {
		t.Errorf("default paid_at/status: %v", p)
	}
	tchExpect(t, "past paid_at", c.pay(sid, 100, "paid_at", "2025-12-31", "note", "  "), 200, "Payment recorded")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_payments WHERE paid_at = '2025-12-31' AND note IS NULL`); n != 1 {
		t.Errorf("past payment with a blank note stored %d times", n)
	}
}

// TestTuitionBalance verifies the balance math: discount, payments, a voided payment kept in the
// history and excluded from every sum, double voids, overpayments, discounts above the fee or the
// remaining balance, and a grade without a fee.
func TestTuitionBalance(t *testing.T) {
	srv, db, admin := l1Server(t, "tuibalance", true)
	c := tuiClient{t, srv, admin}
	sid := tuiStudent(t, db, "مريم حسن", tuiG1, "أ")
	c.fee(tuiG1, 1000000, false)

	d := c.detail(sid, "")
	tuiWant(t, "before any write", tuiFigures(d), tuiG1+" fee=1000000 discount=0 paid=0 remaining=1000000 set=true")
	if len(d["payments"].([]any)) != 0 || d["discount_note"] != nil || d["is_active"] != true {
		t.Errorf("fresh detail: %v", d)
	}
	tchExpect(t, "discount above the fee", c.do("PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 1000001)), 409, "Discount exceeds the fee")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_accounts`); n != 0 {
		t.Fatalf("a refused discount left %d accounts", n)
	}
	r := c.do("PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 100000, "note", " خصم الأخوة "))
	tchExpect(t, "discount", r, 200, "Discount saved")
	d = r.json(t)["data"].(map[string]any)
	tuiWant(t, "after the discount", tuiFigures(d)+" "+tuiV(d["discount_note"]), tuiG1+" fee=1000000 discount=100000 paid=0 remaining=900000 set=true خصم الأخوة")

	first := tuiPaymentID(t, c.pay(sid, 300000))
	r = c.pay(sid, 200000, "note", "القسط الثاني")
	tuiWant(t, "after two payments", tuiFigures(r.json(t)["data"].(map[string]any)), tuiG1+" fee=1000000 discount=100000 paid=500000 remaining=400000 set=true")
	tchExpect(t, "overpayment", c.pay(sid, 400001), 409, "Payment exceeds the remaining balance")

	r = c.do("POST", tuiVoidPath, tuiBody("id", first, "reason", " مبلغ خاطئ "))
	tchExpect(t, "void", r, 200, "Payment voided")
	d = r.json(t)["data"].(map[string]any)
	tuiWant(t, "after the void", tuiFigures(d), tuiG1+" fee=1000000 discount=100000 paid=200000 remaining=700000 set=true")
	pays := d["payments"].([]any)
	if len(pays) != 2 {
		t.Fatalf("payments after the void: %v", pays)
	}
	newest, voided := pays[0].(map[string]any), pays[1].(map[string]any)
	if newest["status"] != "active" || tuiV(newest["amount"]) != "200000" || newest["note"] != "القسط الثاني" || newest["voided_at"] != nil {
		t.Errorf("active payment: %v", newest)
	}
	if voided["status"] != "voided" || tuiV(voided["amount"]) != "300000" || voided["void_reason"] != "مبلغ خاطئ" ||
		!strings.HasSuffix(fmt.Sprint(voided["voided_at"]), "+03:00") || !strings.HasSuffix(fmt.Sprint(voided["created_at"]), "+03:00") {
		t.Errorf("voided payment: %v", voided)
	}
	tchExpect(t, "second void", c.do("POST", tuiVoidPath, tuiBody("id", first, "reason", "مرة أخرى")), 409, "Payment is already voided")
	tchExpect(t, "discount above the remaining balance", c.do("PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 800001)), 409, "Discount exceeds the remaining balance")

	r = c.pay(sid, 700000)
	tuiWant(t, "paid in full", tuiFigures(r.json(t)["data"].(map[string]any)), tuiG1+" fee=1000000 discount=100000 paid=900000 remaining=0 set=true")
	tchExpect(t, "payment on a settled balance", c.pay(sid, 1), 409, "Payment exceeds the remaining balance")
	r = c.do("PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 0))
	tuiWant(t, "discount cleared", tuiFigures(r.json(t)["data"].(map[string]any))+" "+tuiV(r.json(t)["data"].(map[string]any)["discount_note"]),
		tuiG1+" fee=1000000 discount=0 paid=900000 remaining=100000 set=true null")

	rows, _, sum := c.list("")
	tuiWant(t, "list row", tuiFigures(rows[sid]), tuiG1+" fee=1000000 discount=0 paid=900000 remaining=100000 set=true")
	tuiWant(t, "summary", tuiSummary(sum), "fee=1000000 discount=0 collected=900000 remaining=100000 not_set=0")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_payments`); n != 3 {
		t.Errorf("%d payment rows, want 3 (none deleted)", n)
	}

	// A grade without a fee: no figures, and no payment or discount until one is set.
	unset := tuiStudent(t, db, "يوسف علي", tuiG3, "أ")
	tuiWant(t, "fee not set", c.figures(unset, ""), tuiG3+" fee=null discount=0 paid=0 remaining=null set=false")
	tchExpect(t, "payment without a fee", c.pay(unset, 1000), 409, "Set the fee for this grade first")
	tchExpect(t, "discount without a fee", c.do("PUT", tuiDiscPath, tuiBody("student_id", unset, "discount", 0)), 409, "Set the fee for this grade first")
	noGrade := tuiStudent(t, db, "بلا صف", "", "")
	tuiWant(t, "no grade", c.figures(noGrade, ""), "null fee=null discount=0 paid=0 remaining=null set=false")
	tchExpect(t, "payment without a grade", c.pay(noGrade, 1000), 409, "Set the fee for this grade first")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_accounts WHERE student_id IN ($1, $2)`, unset, noGrade); n != 0 {
		t.Errorf("refused writes created %d accounts", n)
	}
}

// TestTuitionList verifies the filters, the summary of the listed rows, and which students are
// listed: active ones, plus inactive ones with an account that year (flagged), never inactive
// ones without an account.
func TestTuitionList(t *testing.T) {
	srv, db, admin := l1Server(t, "tuilist", true)
	c := tuiClient{t, srv, admin}
	c.fee(tuiG1, 1000000, false)
	c.fee(tuiG2, 1200000, false)
	a := tuiStudent(t, db, "أحمد علي", tuiG1, "أ")
	b := tuiStudent(t, db, "سارة_محمد", tuiG1, "ب")
	cc := tuiStudent(t, db, "زينب كريم", tuiG2, "أ")
	d := tuiStudent(t, db, "نور حسين", tuiG3, "أ")
	left := tuiStudent(t, db, "طالب منقول", tuiG1, "أ")
	gone := tuiStudent(t, db, "طالب منسحب", tuiG2, "أ")
	c.pay(a, 1000000)
	c.do("PUT", tuiDiscPath, tuiBody("student_id", cc, "discount", 100000))
	c.pay(cc, 200000)
	c.pay(left, 100000)
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET is_active = false WHERE id IN (%d, %d)`, left, gone))

	rows, ids, sum := c.list("")
	tuiWant(t, "listed students", fmt.Sprint(ids), fmt.Sprint([]int{a, b, cc, d, left}))
	if rows[left]["is_active"] != false || rows[a]["is_active"] != true {
		t.Errorf("is_active flags: left %v, active %v", rows[left]["is_active"], rows[a]["is_active"])
	}
	tuiWant(t, "inactive student with an account", tuiFigures(rows[left]), tuiG1+" fee=1000000 discount=0 paid=100000 remaining=900000 set=true")
	tuiWant(t, "not set row", tuiFigures(rows[d]), tuiG3+" fee=null discount=0 paid=0 remaining=null set=false")
	tuiWant(t, "summary", tuiSummary(sum), "fee=4200000 discount=100000 collected=1300000 remaining=2800000 not_set=1")
	if dt := c.detail(gone, ""); dt["is_active"] != false {
		t.Errorf("inactive student without an account in detail: %v", dt)
	}

	for _, cs := range []struct {
		query string
		want  []int
		sum   string
	}{
		{"grade=" + url.QueryEscape(tuiG1), []int{a, b, left}, "fee=3000000 discount=0 collected=1100000 remaining=1900000 not_set=0"},
		{"grade=" + url.QueryEscape(tuiG1) + "&section=" + url.QueryEscape("ب"), []int{b}, "fee=1000000 discount=0 collected=0 remaining=1000000 not_set=0"},
		{"section=" + url.QueryEscape("أ"), []int{a, cc, d, left}, "fee=3200000 discount=100000 collected=1300000 remaining=1800000 not_set=1"},
		{"search=" + url.QueryEscape("سارة_"), []int{b}, "fee=1000000 discount=0 collected=0 remaining=1000000 not_set=0"},
		{"search=_", []int{b}, "fee=1000000 discount=0 collected=0 remaining=1000000 not_set=0"},
		{"search=%25", nil, "fee=0 discount=0 collected=0 remaining=0 not_set=0"},
		{"search=" + url.QueryEscape("علي"), []int{a}, "fee=1000000 discount=0 collected=1000000 remaining=0 not_set=0"},
		{"has_remaining=true", []int{b, cc, left}, "fee=3200000 discount=100000 collected=300000 remaining=2800000 not_set=0"},
		{"has_remaining=false", []int{a, b, cc, d, left}, "fee=4200000 discount=100000 collected=1300000 remaining=2800000 not_set=1"},
		{"grade=" + url.QueryEscape(tuiG3), []int{d}, "fee=0 discount=0 collected=0 remaining=0 not_set=1"},
	} {
		_, ids, sum := c.list(cs.query)
		tuiWant(t, cs.query+" ids", fmt.Sprint(ids), fmt.Sprint(cs.want))
		tuiWant(t, cs.query+" summary", tuiSummary(sum), cs.sum)
	}

	m := c.ok("fees", "GET", tuiFeesPath, nil)
	var fees []string
	for _, it := range m["data"].([]any) {
		f := it.(map[string]any)
		fees = append(fees, fmt.Sprintf("%s=%s", f["grade"], tuiV(f["fee"])))
	}
	want := []string{tuiG1 + "=1000000", tuiG3 + "=null", tuiG2 + "=1200000"}
	sort.Strings(fees)
	sort.Strings(want)
	tuiWant(t, "fees list", fmt.Sprint(fees), fmt.Sprint(want))
	if m["academic_year"] != tuiYear {
		t.Errorf("fees academic_year %v", m["academic_year"])
	}
}

// TestTuitionNonRetroactive verifies that a balance never changes silently: a fee edit leaves
// accounts with payments alone (apply_to_existing re-prices them, all or nothing), a grade change
// keeps the frozen grade, and a year switch freezes the old year and starts the new one empty.
func TestTuitionNonRetroactive(t *testing.T) {
	srv, db, admin := l1Server(t, "tuiretro", true)
	c := tuiClient{t, srv, admin}
	c.fee(tuiG1, 1000000, false)
	c.fee(tuiG2, 1500000, false)
	s1 := tuiStudent(t, db, "طالب أول", tuiG1, "أ")
	s2 := tuiStudent(t, db, "طالب ثان", tuiG1, "أ")
	s3 := tuiStudent(t, db, "طالب ثالث", tuiG3, "أ")
	c.pay(s1, 400000)

	// (b) A fee edit after payments.
	r := c.fee(tuiG1, 1100000, false)
	if tuiV(r.json(t)["data"].(map[string]any)["updated_accounts"]) != "0" {
		t.Errorf("fee without apply_to_existing: %s", r.body)
	}
	tuiWant(t, "paid student keeps the fee", c.figures(s1, ""), tuiG1+" fee=1000000 discount=0 paid=400000 remaining=600000 set=true")
	tuiWant(t, "unpaid student takes the new fee", c.figures(s2, ""), tuiG1+" fee=1100000 discount=0 paid=0 remaining=1100000 set=true")
	r = c.fee(tuiG1, 1100000, true)
	if tuiV(r.json(t)["data"].(map[string]any)["updated_accounts"]) != "1" {
		t.Errorf("apply_to_existing: %s", r.body)
	}
	tuiWant(t, "re-priced", c.figures(s1, ""), tuiG1+" fee=1100000 discount=0 paid=400000 remaining=700000 set=true")
	tchExpect(t, "apply_to_existing below a payment", c.fee(tuiG1, 300000, true), 409,
		fmt.Sprintf("apply_to_existing would make 1 balances negative (student ids: %d)", s1))
	var many []string
	for i := 0; i < 11; i++ {
		id := tuiStudent(t, db, fmt.Sprintf("طالب %02d", i), tuiG1, "ب")
		c.pay(id, 500000)
		many = append(many, strconv.Itoa(id))
	}
	tchExpect(t, "apply_to_existing names at most 10 students", c.fee(tuiG1, 450000, true), 409,
		"apply_to_existing would make 11 balances negative (student ids: "+strings.Join(many[:10], ", ")+")")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_fees WHERE grade = $1 AND fee = 1100000`, tuiG1); n != 1 {
		t.Errorf("a refused apply_to_existing changed the grade fee")
	}
	tuiWant(t, "refused re-price changes nothing", c.figures(s1, ""), tuiG1+" fee=1100000 discount=0 paid=400000 remaining=700000 set=true")

	// A student moved to another grade after paying keeps the frozen grade until re-priced.
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET grade = '%s' WHERE id = %d`, tuiG2, s1))
	tuiWant(t, "moved student keeps the frozen grade", c.figures(s1, ""), tuiG1+" fee=1100000 discount=0 paid=400000 remaining=700000 set=true")
	c.fee(tuiG2, 1500000, true)
	tuiWant(t, "moved student re-priced", c.figures(s1, ""), tuiG2+" fee=1500000 discount=0 paid=400000 remaining=1100000 set=true")

	// (c) The year switch freezes the old year.
	before := k1Count(t, db, `SELECT COUNT(*) FROM tuition_accounts WHERE academic_year = $1`, tuiYear)
	tchExpect(t, "switch the year", c.setYear(" "+tuiNextYear+" "), 200, "Setting saved")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_accounts WHERE academic_year = $1`, tuiYear); n != before+1 {
		t.Errorf("year switch froze %d accounts, want 1 (only s2; s3's grade has no fee)", n-before)
	}
	if n := k1Count(t, db, `SELECT COUNT(*) FROM settings WHERE setting_key = 'academic_year' AND setting_value = $1`, tuiNextYear); n != 1 {
		t.Fatalf("the setting was not switched")
	}
	old1, old2 := c.figures(s1, tuiYear), c.figures(s2, tuiYear)
	tuiWant(t, "old year s2 frozen", old2, tuiG1+" fee=1100000 discount=0 paid=0 remaining=1100000 set=true")
	tuiWant(t, "old year s3 not set", c.figures(s3, tuiYear), tuiG3+" fee=null discount=0 paid=0 remaining=null set=false")

	// (a) Promotion after the switch never touches the old year.
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET grade = '%s' WHERE id = %d`, tuiG2, s2))
	c.fee(tuiG1, 2000000, true)
	tuiWant(t, "old year s2 after promotion", c.figures(s2, tuiYear), old2)
	tuiWant(t, "new year without a fee", c.figures(s2, ""), tuiG2+" fee=null discount=0 paid=0 remaining=null set=false")

	// Year isolation.
	c.fee(tuiG2, 2000000, false)
	tuiWant(t, "new year live", c.figures(s1, ""), tuiG2+" fee=2000000 discount=0 paid=0 remaining=2000000 set=true")
	c.pay(s1, 300000)
	c.do("PUT", tuiDiscPath, tuiBody("student_id", s1, "discount", 50000))
	tuiWant(t, "old year after new-year writes", c.figures(s1, tuiYear), old1)
	r = c.pay(s2, 100000, "academic_year", tuiYear)
	tchExpect(t, "paying an old-year balance", r, 200, "Payment recorded")
	if r.json(t)["academic_year"] != tuiYear {
		t.Errorf("old-year payment answered for %v", r.json(t)["academic_year"])
	}
	tuiWant(t, "old year s2 after its payment", c.figures(s2, tuiYear), tuiG1+" fee=1100000 discount=0 paid=100000 remaining=1000000 set=true")
	tuiWant(t, "new year s2 unaffected", c.figures(s2, ""), tuiG2+" fee=2000000 discount=0 paid=0 remaining=2000000 set=true")
	tchExpect(t, "old year without an account", c.pay(s3, 1000, "academic_year", tuiYear), 409, "Set the fee for this grade first")
	tchExpect(t, "a future year", c.pay(s1, 1000, "academic_year", "2028-2029"), 409, "Set the fee for this grade first")
	_, oldIDs, _ := c.list("academic_year=" + tuiYear)
	if len(oldIDs) < 3 {
		t.Errorf("old-year list: %v", oldIDs)
	}
	newYear := c.figures(s1, "")
	if !strings.Contains(newYear, "paid=300000") || !strings.Contains(newYear, "discount=50000") {
		t.Errorf("new year s1: %s", newYear)
	}

	// Switching back and forth (a mistyped year) never changes an existing balance.
	snapshot := func() string {
		return c.figures(s1, tuiYear) + "|" + c.figures(s2, tuiYear) + "|" + c.figures(s1, tuiNextYear) + "|" + c.figures(s2, tuiNextYear)
	}
	was := snapshot()
	tchExpect(t, "switch back", c.setYear(tuiYear), 200, "Setting saved")
	tchExpect(t, "same year again", c.setYear(tuiYear), 200, "Setting saved")
	tchExpect(t, "switch forward again", c.setYear(tuiNextYear), 200, "Setting saved")
	tuiWant(t, "balances after switching back and forth", snapshot(), was)
}

// TestTuitionParent verifies GET /api/mobile/tuition: only the parent's own active children,
// only the remaining balance (no fee, discount or payment), and last_updated following changes.
func TestTuitionParent(t *testing.T) {
	srv, db, admin := l1Server(t, "tuiparent", true)
	c := tuiClient{t, srv, admin}
	k1 := p2Student(t, srv, admin, "ابن أول", "ولي أمر أ", "+9647000009201", p2Pin, "4601")
	k2 := p2Student(t, srv, admin, "ابن ثان", "ولي أمر أ", "+9647000009201", "", "4602")
	k3 := p2Student(t, srv, admin, "ابن آخر", "ولي أمر ب", "+9647000009202", p2Pin, "4603")
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET grade = '%s' WHERE id IN (%d, %d)`, tuiG1, k1, k3),
		fmt.Sprintf(`UPDATE students SET grade = '%s' WHERE id = %d`, tuiG3, k2))
	c.fee(tuiG1, 1000000, false)
	tokenA, tokenB := p2Login(t, srv, "+9647000009201", p2Pin), p2Login(t, srv, "+9647000009202", p2Pin)

	get := func(token string) map[int]map[string]any {
		t.Helper()
		r := a14Do(t, srv, "GET", "/api/mobile/tuition", a14Bearer(token), nil)
		if r.status != 200 {
			t.Fatalf("parent tuition: %d %s", r.status, r.body)
		}
		out := map[int]map[string]any{}
		for _, it := range r.json(t)["data"].([]any) {
			m := it.(map[string]any)
			var keys []string
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			tuiWant(t, "parent item keys", strings.Join(keys, ","), "academic_year,fee_set,full_name,last_updated,remaining,student_id")
			out[int(m["student_id"].(float64))] = m
		}
		return out
	}
	a := get(tokenA)
	if len(a) != 2 || a[k1] == nil || a[k2] == nil {
		t.Fatalf("parent A sees %v", a)
	}
	tuiWant(t, "k1", fmt.Sprintf("%s %s %s", tuiV(a[k1]["remaining"]), tuiV(a[k1]["fee_set"]), a[k1]["academic_year"]), "1000000 true "+tuiYear)
	tuiWant(t, "k2 not set", fmt.Sprintf("%s %s %s", tuiV(a[k2]["remaining"]), tuiV(a[k2]["fee_set"]), tuiV(a[k2]["last_updated"])), "null false null")
	feeTime := fmt.Sprint(a[k1]["last_updated"])
	if !strings.HasSuffix(feeTime, "+03:00") {
		t.Errorf("last_updated before payments: %q", feeTime)
	}
	if b := get(tokenB); len(b) != 1 || b[k3] == nil {
		t.Fatalf("parent B sees %v", b)
	}

	time.Sleep(1100 * time.Millisecond)
	pid := tuiPaymentID(t, c.pay(k1, 300000))
	a = get(tokenA)
	paidTime := fmt.Sprint(a[k1]["last_updated"])
	if tuiV(a[k1]["remaining"]) != "700000" || paidTime <= feeTime {
		t.Errorf("after a payment: remaining %v, last_updated %s (was %s)", a[k1]["remaining"], paidTime, feeTime)
	}
	time.Sleep(1100 * time.Millisecond)
	c.do("POST", tuiVoidPath, tuiBody("id", pid, "reason", "خطأ"))
	a = get(tokenA)
	if tuiV(a[k1]["remaining"]) != "1000000" || fmt.Sprint(a[k1]["last_updated"]) <= paidTime {
		t.Errorf("after a void: remaining %v, last_updated %v (was %s)", a[k1]["remaining"], a[k1]["last_updated"], paidTime)
	}
	if b := get(tokenB); tuiV(b[k3]["remaining"]) != "1000000" {
		t.Errorf("another parent's child changed: %v", b[k3])
	}
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET is_active = false WHERE id = %d`, k2))
	if a = get(tokenA); len(a) != 1 || a[k1] == nil {
		t.Errorf("an inactive child is listed: %v", a)
	}

	tchExpect(t, "no token", a14Do(t, srv, "GET", "/api/mobile/tuition", nil, nil), 401, "")
	tchExpect(t, "bad token", a14Do(t, srv, "GET", "/api/mobile/tuition", a14Bearer("not-a-token"), nil), 401, "")
	tchExpect(t, "admin token", a14Do(t, srv, "GET", "/api/mobile/tuition", a14Bearer(admin), nil), 403, "")
	for _, p := range []string{tuiPath, tuiFeesPath, tuiPath + fmt.Sprintf("/student?student_id=%d", k1)} {
		tchExpect(t, "parent token on "+p, a14Do(t, srv, "GET", p, a14Bearer(tokenA), nil), 403, "")
	}
	tchExpect(t, "parent token on a payment", a14Do(t, srv, "POST", tuiPayPath, a14Bearer(tokenA), tuiBody("student_id", k1, "amount", 1)), 403, "")
}

// TestTuitionActivityLog verifies that every tuition write and the year change are recorded with
// identifiers and counts only: no name, amount, note or reason.
func TestTuitionActivityLog(t *testing.T) {
	srv, db, admin := l1Server(t, "tuiactivity", true)
	c := tuiClient{t, srv, admin}
	sid := tuiStudent(t, db, "اسم سري للطالب", tuiG1, "أ")
	tuiStudent(t, db, "طالب بلا دفعات", tuiG1, "أ")
	// Activity rows are inserted in the background after each answer, so wait for every row
	// before the next request: only then does the id order follow the request order.
	logged := 1
	recorded := func() { t.Helper(); logged++; l1WaitCount(t, db, logged) }
	fee := c.fee(tuiG1, 1234567, false)
	recorded()
	feeID := k1Count(t, db, `SELECT id FROM tuition_fees`)
	c.do("PUT", tuiDiscPath, tuiBody("student_id", sid, "discount", 34567, "note", "ملاحظة خصم سرية"))
	recorded()
	pid := tuiPaymentID(t, c.pay(sid, 765432, "note", "ملاحظة دفعة سرية"))
	recorded()
	c.do("POST", tuiVoidPath, tuiBody("id", pid, "reason", "سبب إلغاء سري"))
	recorded()
	c.setYear(tuiNextYear)
	recorded()
	if fee.status != 200 {
		t.Fatalf("fee: %d %s", fee.status, fee.body)
	}
	rows := l1Rows(t, db, `action <> 'login.success'`)
	var got []string
	for _, r := range rows {
		got = append(got, fmt.Sprintf("%s %d %s:%s %s", r.Action, r.Status, l1Null(r.TargetType), l1Null(r.TargetID), l1Null(r.Detail)))
		line := r.String()
		for _, secret := range []string{"اسم سري", "1234567", "34567", "765432", "ملاحظة", "سبب", tuiG1} {
			if strings.Contains(line, secret) {
				t.Errorf("activity row contains %q: %s", secret, line)
			}
		}
	}
	tuiWant(t, "activity rows", strings.Join(got, "\n"), strings.Join([]string{
		fmt.Sprintf("tuition.fee_set 200 fee:%d year=%s accounts=0", feeID, tuiYear),
		fmt.Sprintf("tuition.discount_set 200 student:%d year=%s", sid, tuiYear),
		fmt.Sprintf("tuition.payment_create 200 payment:%d student=%d year=%s", pid, sid, tuiYear),
		fmt.Sprintf("tuition.payment_void 200 payment:%d student=%d", pid, sid),
		"settings.update 200 setting:academic_year accounts=1",
	}, "\n"))
}

// TestTuitionAcademicYearMissing verifies that a missing or invalid academic_year setting fails
// loudly (an ERROR log and the standard 500) instead of falling back to a default.
func TestTuitionAcademicYearMissing(t *testing.T) {
	srv, db, admin := l1Server(t, "tuinoyear", true)
	c := tuiClient{t, srv, admin}
	sid := p2Student(t, srv, admin, "طالب", "ولي أمر", "+9647000009211", p2Pin, "4611")
	token := p2Login(t, srv, "+9647000009211", p2Pin)
	internal := `{"message":"Internal server error","status":"error"}` + "\n"
	check := func(state string) {
		t.Helper()
		for _, cs := range []struct {
			method, path string
			header       map[string]string
			body         any
		}{
			{"GET", tuiPath, l1Header(admin), nil},
			{"GET", tuiFeesPath, l1Header(admin), nil},
			{"GET", fmt.Sprintf("%s/student?student_id=%d", tuiPath, sid), l1Header(admin), nil},
			{"PUT", tuiFeesPath, l1Header(admin), tuiBody("grade", tuiG1, "fee", 1000)},
			{"POST", tuiPayPath, l1Header(admin), tuiBody("student_id", sid, "amount", 1)},
			{"PUT", "/api/admin/settings", l1Header(admin), tuiBody("key", "academic_year", "value", tuiYear)},
			{"GET", "/api/mobile/tuition", a14Bearer(token), nil},
		} {
			r := a14Do(t, srv, cs.method, cs.path, cs.header, cs.body)
			if r.status != 500 || string(r.body) != internal {
				t.Errorf("%s: %s %s: %d %s, want 500", state, cs.method, cs.path, r.status, r.body)
			}
		}
	}
	errorLines := func() int {
		n := 0
		for _, line := range strings.Split(srv.out.String(), "\n") {
			if strings.Contains(line, `"level":"ERROR"`) && strings.Contains(line, "the academic_year setting is missing or invalid") {
				n++
			}
		}
		return n
	}
	k1Exec(t, db, `DELETE FROM settings WHERE setting_key = 'academic_year'`)
	check("missing")
	// The server logs before it answers, but its output reaches the test through a pipe: wait
	// for the lines (bounded) instead of counting them at once.
	for deadline := time.Now().Add(10 * time.Second); errorLines() < 7 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := errorLines(); n != 7 {
		t.Errorf("%d ERROR lines for the missing setting, want 7:\n%s", n, srv.out.String())
	}
	k1Exec(t, db, `INSERT INTO settings (setting_key, setting_value) VALUES ('academic_year', '2026')`)
	check("invalid")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM tuition_fees`) + k1Count(t, db, `SELECT COUNT(*) FROM tuition_accounts`); n != 0 {
		t.Errorf("writes without a valid academic year stored %d rows", n)
	}
	k1Exec(t, db, `UPDATE settings SET setting_value = '2026-2027' WHERE setting_key = 'academic_year'`)
	tchExpect(t, "restored", c.fee(tuiG1, 1000, false), 200, "Fee saved")
}

// TestTuitionMigration runs 000035 down and up again (twice, so it is idempotent).
func TestTuitionMigration(t *testing.T) {
	db, _ := setupThrowawayDB(t, "tuimigr")
	up, down := a14Migration(t, "000035_create_tuition.up.sql"), a14Migration(t, "000035_create_tuition.down.sql")
	state := func() string {
		return fmt.Sprintf("tables=%d year=%d",
			k1Count(t, db, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('tuition_fees', 'tuition_accounts', 'tuition_payments')`),
			k1Count(t, db, `SELECT COUNT(*) FROM settings WHERE setting_key = 'academic_year' AND setting_value = '2026-2027'`))
	}
	tuiWant(t, "after up", state(), "tables=3 year=1")
	k1Exec(t, db, down)
	tuiWant(t, "after down", state(), "tables=0 year=0")
	k1Exec(t, db, up, up)
	tuiWant(t, "after up again (twice)", state(), "tables=3 year=1")
	k1Exec(t, db, `UPDATE settings SET setting_value = '2027-2028' WHERE setting_key = 'academic_year'`, up)
	if n := k1Count(t, db, `SELECT COUNT(*) FROM settings WHERE setting_key = 'academic_year' AND setting_value = '2027-2028'`); n != 1 {
		t.Errorf("re-running the migration overwrote the academic year")
	}
}
