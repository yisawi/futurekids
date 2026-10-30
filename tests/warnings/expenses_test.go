package warnings

import (
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"future_kids/internal/handlers"
)

const (
	expPath     = "/api/admin/expenses"
	expCatPath  = "/api/admin/expenses/categories"
	expVoidPath = "/api/admin/expenses/void"
	expSumPath  = "/api/admin/expenses/summary"
)

// expSeeded are the migration's categories, in id order; only the first is the salary category.
var expSeeded = []string{"رواتب", "إيجار", "مولد كهرباء", "تواصل اجتماعي", "كافتريا", "إنترنت", "أخرى"}

// expCategory returns the id of a category by name.
func expCategory(t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	return k1Count(t, db, `SELECT id FROM expense_categories WHERE name = $1`, name)
}

func (c tuiClient) expense(kv ...any) a14Response {
	c.t.Helper()
	return c.do("POST", expPath, tuiBody(kv...))
}

// expRecord records an expense and returns its id, failing the test unless it gets 200.
func (c tuiClient) expRecord(kv ...any) int64 {
	c.t.Helper()
	r := c.expense(kv...)
	if r.status != 200 {
		c.t.Fatalf("record expense %v: %d %s", kv, r.status, r.body)
	}
	return int64(r.json(c.t)["data"].(map[string]any)["id"].(float64))
}

// expList returns the rows of GET /api/admin/expenses?query as "id amount status" and the total.
func (c tuiClient) expList(query string) ([]string, string) {
	c.t.Helper()
	m := c.ok("expenses "+query, "GET", expPath+"?"+query, nil)
	var rows []string
	for _, it := range m["data"].([]any) {
		e := it.(map[string]any)
		rows = append(rows, fmt.Sprintf("%s %s %s", tuiV(e["id"]), tuiV(e["amount"]), e["status"]))
	}
	return rows, tuiV(m["summary"].(map[string]any)["total"])
}

func expTotals(m map[string]any) string {
	return fmt.Sprintf("expenses=%s collected=%s difference=%s", tuiV(m["expenses"]), tuiV(m["collected"]), tuiV(m["difference"]))
}

// expSummary renders a summary: the totals, then each category with a total, then each month.
func expSummary(m map[string]any) string {
	var b strings.Builder
	fmt.Fprintf(&b, "year=%s category=%s %s", m["academic_year"], tuiV(m["category_id"]), expTotals(m["totals"].(map[string]any)))
	for _, it := range m["by_category"].([]any) {
		cat := it.(map[string]any)
		fmt.Fprintf(&b, "|%s=%s", cat["name"], tuiV(cat["total"]))
	}
	for _, it := range m["by_month"].([]any) {
		mo := it.(map[string]any)
		fmt.Fprintf(&b, "|%s %s", mo["month"], expTotals(mo))
	}
	return b.String()
}

// TestExpensesValidation verifies the 400s (amounts, dates, lengths, ids, months, academic years,
// JSON), the 404s, the request limits and auth, and that a refused request writes nothing.
func TestExpensesValidation(t *testing.T) {
	srv, db, admin := l1Server(t, "expvalid", true)
	c := tuiClient{t, srv, admin}
	rent := expCategory(t, db, "إيجار")
	long := strings.Repeat("ب", 201)
	rangeMsg := "amount must be from 1 to 1000000000"
	for _, cs := range []struct {
		name, method, path string
		body               any
		status             int
		msg                string
	}{
		{"amount 0", "POST", expPath, tuiBody("category_id", rent, "amount", 0), 400, rangeMsg},
		{"amount negative", "POST", expPath, tuiBody("category_id", rent, "amount", -1), 400, rangeMsg},
		{"amount huge", "POST", expPath, tuiBody("category_id", rent, "amount", 1000000001), 400, rangeMsg},
		{"amount missing", "POST", expPath, tuiBody("category_id", rent), 400, "amount is required"},
		{"amount not an integer", "POST", expPath, tuiBody("category_id", rent, "amount", 1.5), 400, "Invalid request body"},
		{"amount a string", "POST", expPath, tuiBody("category_id", rent, "amount", "100"), 400, "Invalid request body"},
		{"amount beyond int64", "POST", expPath, fmt.Sprintf(`{"category_id":%d,"amount":1e30}`, rent), 400, "Invalid request body"},
		{"category_id missing", "POST", expPath, tuiBody("amount", 100), 400, handlers.ExpenseCategoryIDMessage},
		{"teacher_id 0", "POST", expPath, tuiBody("category_id", rent, "amount", 100, "teacher_id", 0), 400, handlers.ExpenseTeacherIDMessage},
		{"expense_date tomorrow", "POST", expPath, tuiBody("category_id", rent, "amount", 100, "expense_date", "2026-09-25"), 400, handlers.ExpenseDateFutureMessage},
		{"expense_date month 13", "POST", expPath, tuiBody("category_id", rent, "amount", 100, "expense_date", "2026-13-01"), 400, handlers.ExpenseDateMessage},
		{"expense_date day first", "POST", expPath, tuiBody("category_id", rent, "amount", 100, "expense_date", "24-09-2026"), 400, handlers.ExpenseDateMessage},
		{"note 201 characters", "POST", expPath, tuiBody("category_id", rent, "amount", 100, "note", long), 400, "note must be at most 200 characters"},
		{"academic_year in the body", "POST", expPath, tuiBody("category_id", rent, "amount", 100, "academic_year", "2026-2028"), 400, handlers.AcademicYearMessage},
		{"malformed JSON", "POST", expPath, "{", 400, "Invalid request body"},
		{"unknown category", "POST", expPath, tuiBody("category_id", 999999, "amount", 100), 404, "Category not found"},
		{"void id missing", "POST", expVoidPath, tuiBody("reason", "x"), 400, handlers.ExpenseIDMessage},
		{"void reason missing", "POST", expVoidPath, tuiBody("id", 1, "reason", "  "), 400, "reason is required"},
		{"void reason 201 characters", "POST", expVoidPath, tuiBody("id", 1, "reason", long), 400, "reason must be at most 200 characters"},
		{"void unknown expense", "POST", expVoidPath, tuiBody("id", 999999, "reason", "x"), 404, "Expense not found"},
		{"category name blank", "POST", expCatPath, tuiBody("name", "   "), 400, "name is required"},
		{"category name 51 characters", "POST", expCatPath, tuiBody("name", strings.Repeat("ب", 51)), 400, "name must be at most 50 characters"},
		{"category malformed JSON", "POST", expCatPath, "{", 400, "Invalid request body"},
		{"list month 2026-9", "GET", expPath + "?month=2026-9", nil, 400, "month must be formatted as YYYY-MM"},
		{"list month 2026/09", "GET", expPath + "?month=" + url.QueryEscape("2026/09"), nil, 400, "month must be formatted as YYYY-MM"},
		{"list category_id", "GET", expPath + "?category_id=x", nil, 400, handlers.ExpenseCategoryIDMessage},
		{"list teacher_id", "GET", expPath + "?teacher_id=-1", nil, 400, handlers.ExpenseTeacherIDMessage},
		{"list academic_year", "GET", expPath + "?academic_year=26-27", nil, 400, handlers.AcademicYearMessage},
		{"summary academic_year", "GET", expSumPath + "?academic_year=2027-2026", nil, 400, handlers.AcademicYearMessage},
		{"summary category_id", "GET", expSumPath + "?category_id=0", nil, 400, handlers.ExpenseCategoryIDMessage},
		{"summary unknown category", "GET", expSumPath + "?category_id=999999", nil, 404, "Category not found"},
	} {
		tchExpect(t, cs.name, c.do(cs.method, cs.path, cs.body), cs.status, cs.msg)
	}
	pad := `{"pad":"` + strings.Repeat("a", 70<<10) + `"}`
	for _, p := range []string{expPath, expVoidPath, expCatPath} {
		tchExpect(t, "oversized body "+p, c.do("POST", p, pad), 413, "")
		tchExpect(t, "no token "+p, a14Do(t, srv, "POST", p, nil, tuiBody()), 401, "")
	}
	p2Student(t, srv, admin, "طالب", "ولي أمر", "+9647000009301", p2Pin, "4701")
	parent := p2Login(t, srv, "+9647000009301", p2Pin)
	for _, p := range []string{expPath, expCatPath, expSumPath} {
		tchExpect(t, "no token "+p, a14Do(t, srv, "GET", p, nil, nil), 401, "")
		tchExpect(t, "parent token "+p, a14Do(t, srv, "GET", p, a14Bearer(parent), nil), 403, "")
	}
	tchExpect(t, "parent token record", a14Do(t, srv, "POST", expPath, a14Bearer(parent), tuiBody("category_id", rent, "amount", 1)), 403, "")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM expenses`) + k1Count(t, db, `SELECT COUNT(*) FROM expense_categories`); n != len(expSeeded) {
		t.Fatalf("refused requests wrote rows: %d", n-len(expSeeded))
	}

	r := c.expense("category_id", rent, "amount", 1000000000)
	tchExpect(t, "amount 1,000,000,000, default date", r, 200, "Expense recorded")
	d := r.json(t)["data"].(map[string]any)
	tuiWant(t, "recorded expense", fmt.Sprintf("%s %s %s %s %s %v %v", tuiV(d["amount"]), d["expense_date"], d["academic_year"], d["status"], d["category"].(map[string]any)["name"], d["note"], d["teacher"]),
		"1000000000 "+k1Today+" "+tuiYear+" active إيجار <nil> <nil>")
	if !strings.HasSuffix(fmt.Sprint(d["created_at"]), "+03:00") || d["voided_at"] != nil || d["void_reason"] != nil {
		t.Errorf("recorded expense: %v", d)
	}
	tchExpect(t, "past date, blank note", c.expense("category_id", rent, "amount", 5, "expense_date", "2025-12-31", "note", "  "), 200, "Expense recorded")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM expenses WHERE expense_date = '2025-12-31' AND note IS NULL`); n != 1 {
		t.Errorf("past expense with a blank note stored %d times", n)
	}
	tchExpect(t, "a future academic year", c.expense("category_id", rent, "amount", 5, "academic_year", tuiNextYear), 200, "Expense recorded")
}

// TestExpensesCategories verifies the seeded categories (only رواتب is the salary one) and adding
// one: trimmed with inner spaces collapsed, never a salary category, unique ignoring case and spaces.
func TestExpensesCategories(t *testing.T) {
	srv, db, admin := l1Server(t, "expcats", true)
	c := tuiClient{t, srv, admin}
	list := func() []string {
		var out []string
		for _, it := range c.ok("categories", "GET", expCatPath, nil)["data"].([]any) {
			cat := it.(map[string]any)
			out = append(out, fmt.Sprintf("%s:%v", cat["name"], cat["is_salary"]))
		}
		return out
	}
	var want []string
	for i, n := range expSeeded {
		want = append(want, fmt.Sprintf("%s:%v", n, i == 0))
	}
	tuiWant(t, "seeded categories", strings.Join(list(), ","), strings.Join(want, ","))

	r := c.do("POST", expCatPath, tuiBody("name", "  صيانة   المبنى "))
	tchExpect(t, "add a category", r, 200, "Category added")
	d := r.json(t)["data"].(map[string]any)
	if d["name"] != "صيانة المبنى" || d["is_salary"] != false || tuiV(d["id"]) != fmt.Sprint(expCategory(t, db, "صيانة المبنى")) {
		t.Errorf("added category: %v", d)
	}
	tchExpect(t, "add a Latin category", c.do("POST", expCatPath, tuiBody("name", "Internet Fee")), 200, "Category added")
	for _, dup := range []string{"صيانة المبنى", "صيانة  المبنى", " إنترنت ", "رواتب", "internet   fee", "INTERNET FEE"} {
		tchExpect(t, "duplicate "+dup, c.do("POST", expCatPath, tuiBody("name", dup)), 409, "name is already used by another category")
	}
	tuiWant(t, "categories after adding", strings.Join(list(), ","), strings.Join(append(want, "صيانة المبنى:false", "Internet Fee:false"), ","))
	if n := k1Count(t, db, `SELECT COUNT(*) FROM expense_categories WHERE is_salary`); n != 1 {
		t.Errorf("%d salary categories, want 1", n)
	}
}

// TestExpensesSalaryLink verifies that only the salary category may name a teacher (any status,
// unknown is 404), that a salary may name none, and that teacher responses never show expenses.
func TestExpensesSalaryLink(t *testing.T) {
	srv, db, admin := l1Server(t, "expsalary", true)
	c := tuiClient{t, srv, admin}
	salary, rent := expCategory(t, db, "رواتب"), expCategory(t, db, "إيجار")
	teacher := tchCreate(t, srv, admin, tchBody("phone", "07000009401", "rfid_tag", "41"))
	retired := tchCreate(t, srv, admin, tchBody("phone", "07000009402", "first_name", "سعاد", "gender", "female"))
	k1Exec(t, db, fmt.Sprintf(`UPDATE teachers SET status = 'retired' WHERE id = %d`, retired))
	keys := func(r a14Response) string {
		var out []string
		for _, it := range tchList(t, r) {
			var ks []string
			for k := range it {
				ks = append(ks, k)
			}
			sort.Strings(ks)
			out = append(out, strings.Join(ks, ","))
		}
		return strings.Join(out, "|")
	}
	teacherKeys := "created_at,father_name,first_name,full_name,gender,grandfather_name,id,notes,phone,rfid_tag,specialization,status,updated_at"
	before := a14Do(t, srv, "GET", tchPath, l1Header(admin), nil)

	r := c.expense("category_id", salary, "amount", 750000, "teacher_id", teacher, "note", "راتب أيلول")
	tchExpect(t, "salary for a teacher", r, 200, "Expense recorded")
	if tt := r.json(t)["data"].(map[string]any)["teacher"].(map[string]any); tuiV(tt["id"]) != fmt.Sprint(teacher) || tt["full_name"] != "أحمد علي حسن" {
		t.Errorf("salary teacher: %v", tt)
	}
	tchExpect(t, "salary for a retired teacher", c.expense("category_id", salary, "amount", 300000, "teacher_id", retired), 200, "Expense recorded")
	r = c.expense("category_id", salary, "amount", 400000, "note", "حارس المدرسة")
	tchExpect(t, "salary without a teacher", r, 200, "Expense recorded")
	if r.json(t)["data"].(map[string]any)["teacher"] != nil {
		t.Errorf("a salary without a teacher shows one: %s", r.body)
	}
	tchExpect(t, "teacher on another category", c.expense("category_id", rent, "amount", 1, "teacher_id", teacher), 409, "teacher_id is allowed only for the salary category")
	tchExpect(t, "unknown teacher", c.expense("category_id", salary, "amount", 1, "teacher_id", 999999), 404, handlers.TeacherNotFoundMessage)
	if n := k1Count(t, db, `SELECT COUNT(*) FROM expenses`); n != 3 {
		t.Errorf("%d expenses, want 3 (refused ones write nothing)", n)
	}
	rows, total := c.expList(fmt.Sprintf("teacher_id=%d", teacher))
	if len(rows) != 1 || total != "750000" {
		t.Errorf("teacher filter: %v total %s", rows, total)
	}

	after := a14Do(t, srv, "GET", tchPath, l1Header(admin), nil)
	one := a14Do(t, srv, "GET", tchPath+fmt.Sprintf("?id=%d", teacher), l1Header(admin), nil)
	tuiWant(t, "teacher list keys", keys(after), teacherKeys+"|"+teacherKeys)
	tuiWant(t, "one teacher keys", keys(one), teacherKeys)
	if string(after.body) != string(before.body) {
		t.Errorf("salary expenses changed the teachers list:\n%s\n%s", before.body, after.body)
	}
	for _, body := range [][]byte{after.body, one.body} {
		for _, leak := range []string{"750000", "300000", "salary", "expense", "amount"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("teacher response contains %q: %s", leak, body)
			}
		}
	}
}

// TestExpensesVoidAndList verifies voiding (reason required, kept in the history, excluded from
// totals, voiding twice refused), the newest-first order and the list filters with their totals.
func TestExpensesVoidAndList(t *testing.T) {
	srv, db, admin := l1Server(t, "explist", true)
	c := tuiClient{t, srv, admin}
	salary, rent, gen := expCategory(t, db, "رواتب"), expCategory(t, db, "إيجار"), expCategory(t, db, "مولد كهرباء")
	teacher := tchCreate(t, srv, admin, tchBody("phone", "07000009411"))
	a := c.expRecord("category_id", rent, "amount", 500000, "expense_date", "2026-08-01")
	b := c.expRecord("category_id", salary, "amount", 250000, "expense_date", "2026-08-30", "teacher_id", teacher)
	cc := c.expRecord("category_id", salary, "amount", 250000, "expense_date", "2026-09-20", "teacher_id", teacher)
	d := c.expRecord("category_id", gen, "amount", 70000, "expense_date", "2026-09-20")
	e := c.expRecord("category_id", rent, "amount", 500000, "expense_date", "2026-09-01")

	r := c.do("POST", expVoidPath, tuiBody("id", d, "reason", " رقم خاطئ "))
	tchExpect(t, "void", r, 200, "Expense voided")
	v := r.json(t)["data"].(map[string]any)
	if v["status"] != "voided" || v["void_reason"] != "رقم خاطئ" || !strings.HasSuffix(fmt.Sprint(v["voided_at"]), "+03:00") || tuiV(v["amount"]) != "70000" {
		t.Errorf("voided expense: %v", v)
	}
	tchExpect(t, "void twice", c.do("POST", expVoidPath, tuiBody("id", d, "reason", "مرة أخرى")), 409, "Expense is already voided")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM expenses`); n != 5 {
		t.Errorf("%d expenses after the void, want 5 (nothing deleted)", n)
	}

	row := func(id int64, amount, status string) string { return fmt.Sprintf("%d %s %s", id, amount, status) }
	for _, cs := range []struct {
		query string
		rows  []string
		total string
	}{
		{"", []string{row(d, "70000", "voided"), row(cc, "250000", "active"), row(e, "500000", "active"), row(b, "250000", "active"), row(a, "500000", "active")}, "1500000"},
		{fmt.Sprintf("category_id=%d", salary), []string{row(cc, "250000", "active"), row(b, "250000", "active")}, "500000"},
		{"month=2026-09", []string{row(d, "70000", "voided"), row(cc, "250000", "active"), row(e, "500000", "active")}, "750000"},
		{fmt.Sprintf("month=2026-08&category_id=%d", salary), []string{row(b, "250000", "active")}, "250000"},
		{fmt.Sprintf("teacher_id=%d&month=2026-09", teacher), []string{row(cc, "250000", "active")}, "250000"},
		{fmt.Sprintf("category_id=%d", gen), []string{row(d, "70000", "voided")}, "0"},
		{"category_id=999999", nil, "0"},
		{"academic_year=" + tuiNextYear, nil, "0"},
	} {
		rows, total := c.expList(cs.query)
		tuiWant(t, "list "+cs.query, strings.Join(rows, ",")+" total="+total, strings.Join(cs.rows, ",")+" total="+cs.total)
	}
}

// TestExpensesSummary verifies the year report: totals, every category, every month with expenses
// or collections, and the difference using non-voided tuition payments of every student (inactive
// ones included); the category filter (expenses only, collected and difference null); and that
// each academic year only counts its own expenses and tuition.
func TestExpensesSummary(t *testing.T) {
	srv, db, admin := l1Server(t, "expsummary", true)
	c := tuiClient{t, srv, admin}
	salary, rent, internet, gen := expCategory(t, db, "رواتب"), expCategory(t, db, "إيجار"), expCategory(t, db, "إنترنت"), expCategory(t, db, "مولد كهرباء")
	teacher := tchCreate(t, srv, admin, tchBody("phone", "07000009421"))
	s1, s2 := tuiStudent(t, db, "طالب أول", tuiG1, "أ"), tuiStudent(t, db, "طالب منقول", tuiG1, "أ")
	c.fee(tuiG1, 1000000, false)
	c.pay(s1, 300000, "paid_at", "2026-08-10")
	c.pay(s1, 200000, "paid_at", "2026-09-01")
	c.do("POST", tuiVoidPath, tuiBody("id", tuiPaymentID(t, c.pay(s1, 50000, "paid_at", "2026-09-02")), "reason", "خطأ"))
	c.pay(s2, 100000, "paid_at", "2026-09-03")
	k1Exec(t, db, fmt.Sprintf(`UPDATE students SET is_active = false WHERE id = %d`, s2))

	c.expRecord("category_id", rent, "amount", 400000, "expense_date", "2026-07-15")
	c.expRecord("category_id", salary, "amount", 250000, "expense_date", "2026-09-05", "teacher_id", teacher)
	c.expRecord("category_id", salary, "amount", 150000, "expense_date", "2026-09-06", "note", "حارس المدرسة")
	c.expRecord("category_id", internet, "amount", 30000, "expense_date", "2026-09-07")
	c.do("POST", expVoidPath, tuiBody("id", c.expRecord("category_id", gen, "amount", 70000, "expense_date", "2026-09-08"), "reason", "خطأ"))

	// Another year: its own expense and tuition, never counted in 2026-2027.
	c.expRecord("category_id", rent, "amount", 999000, "expense_date", "2026-07-01", "academic_year", "2025-2026")
	k1Exec(t, db, fmt.Sprintf(`
		WITH a AS (INSERT INTO tuition_accounts (student_id, academic_year, grade, fee) VALUES (%d, '2025-2026', 'G', 900000) RETURNING id)
		INSERT INTO tuition_payments (account_id, amount, paid_at) SELECT id, 888000, '2026-07-02' FROM a`, s1))

	cats := "|رواتب=400000|إيجار=400000|مولد كهرباء=0|تواصل اجتماعي=0|كافتريا=0|إنترنت=30000|أخرى=0"
	tuiWant(t, "summary", expSummary(c.ok("summary", "GET", expSumPath, nil)),
		"year=2026-2027 category=null expenses=830000 collected=600000 difference=-230000"+cats+
			"|2026-07 expenses=400000 collected=0 difference=-400000"+
			"|2026-08 expenses=0 collected=300000 difference=300000"+
			"|2026-09 expenses=430000 collected=300000 difference=-130000")
	tuiWant(t, "salaries only", expSummary(c.ok("salaries", "GET", fmt.Sprintf("%s?category_id=%d", expSumPath, salary), nil)),
		fmt.Sprintf("year=2026-2027 category=%d expenses=400000 collected=null difference=null|رواتب=400000|2026-09 expenses=400000 collected=null difference=null", salary))
	tuiWant(t, "a category without expenses", expSummary(c.ok("generator", "GET", fmt.Sprintf("%s?category_id=%d", expSumPath, gen), nil)),
		fmt.Sprintf("year=2026-2027 category=%d expenses=0 collected=null difference=null|مولد كهرباء=0", gen))
	tuiWant(t, "the other year", expSummary(c.ok("2025-2026", "GET", expSumPath+"?academic_year=2025-2026", nil)),
		"year=2025-2026 category=null expenses=999000 collected=888000 difference=-111000|رواتب=0|إيجار=999000|مولد كهرباء=0|تواصل اجتماعي=0|كافتريا=0|إنترنت=0|أخرى=0"+
			"|2026-07 expenses=999000 collected=888000 difference=-111000")
	tuiWant(t, "an empty year", expSummary(c.ok("2027-2028", "GET", expSumPath+"?academic_year="+tuiNextYear, nil)),
		"year=2027-2028 category=null expenses=0 collected=0 difference=0|رواتب=0|إيجار=0|مولد كهرباء=0|تواصل اجتماعي=0|كافتريا=0|إنترنت=0|أخرى=0")
	if rows, total := c.expList("academic_year=2025-2026"); len(rows) != 1 || total != "999000" {
		t.Errorf("2025-2026 list: %v total %s", rows, total)
	}
}

// TestExpensesActivityLog verifies that the three writes are recorded with ids and counts only:
// no name, amount, note, reason or teacher id. Each request waits for its row, so ids follow the
// request order (rows are inserted in the background after the answer).
func TestExpensesActivityLog(t *testing.T) {
	srv, db, admin := l1Server(t, "expactivity", true)
	c := tuiClient{t, srv, admin}
	teacher := tchCreate(t, srv, admin, tchBody("phone", "07000009431", "first_name", "اسمسري"))
	salary := expCategory(t, db, "رواتب")
	count := k1Count(t, db, `SELECT COUNT(*) FROM admin_activity_log`)
	wait := func() { t.Helper(); count++; l1WaitCount(t, db, count) }
	r := c.do("POST", expCatPath, tuiBody("name", "فئة سرية"))
	wait()
	category := tuiV(r.json(t)["data"].(map[string]any)["id"])
	id := c.expRecord("category_id", salary, "amount", 1234567, "teacher_id", teacher, "note", "ملاحظة سرية")
	wait()
	c.do("POST", expVoidPath, tuiBody("id", id, "reason", "سبب سري"))
	wait()
	var got []string
	for _, row := range l1Rows(t, db, `action LIKE 'expense.%'`) {
		got = append(got, fmt.Sprintf("%s %d %s:%s %s", row.Action, row.Status, l1Null(row.TargetType), l1Null(row.TargetID), l1Null(row.Detail)))
		for _, secret := range []string{"سري", "1234567", "teacher", "اسمسري"} {
			if strings.Contains(row.String(), secret) {
				t.Errorf("activity row contains %q: %s", secret, row)
			}
		}
	}
	tuiWant(t, "activity rows", strings.Join(got, "\n"), strings.Join([]string{
		"expense.category_create 200 category:" + category + " -",
		fmt.Sprintf("expense.create 200 expense:%d category=%d year=%s", id, salary, tuiYear),
		fmt.Sprintf("expense.void 200 expense:%d -", id),
	}, "\n"))
}

// TestExpensesAcademicYearMissing verifies that a missing or invalid academic_year setting fails
// loudly (an ERROR log and the standard 500) on the routes that use the year, and writes nothing.
func TestExpensesAcademicYearMissing(t *testing.T) {
	srv, db, admin := l1Server(t, "expnoyear", true)
	c := tuiClient{t, srv, admin}
	rent := expCategory(t, db, "إيجار")
	internal := `{"message":"Internal server error","status":"error"}` + "\n"
	check := func(state string) {
		t.Helper()
		for _, cs := range []struct {
			method, path string
			body         any
		}{
			{"GET", expPath, nil},
			{"GET", expSumPath, nil},
			{"POST", expPath, tuiBody("category_id", rent, "amount", 1)},
			{"POST", expPath, tuiBody("category_id", rent, "amount", 1, "academic_year", tuiYear)},
		} {
			if r := c.do(cs.method, cs.path, cs.body); r.status != 500 || string(r.body) != internal {
				t.Errorf("%s: %s %s: %d %s, want 500", state, cs.method, cs.path, r.status, r.body)
			}
		}
	}
	lines := func() int {
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
	for deadline := time.Now().Add(10 * time.Second); lines() < 4 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	if n := lines(); n != 4 {
		t.Errorf("%d ERROR lines for the missing setting, want 4:\n%s", n, srv.out.String())
	}
	k1Exec(t, db, `INSERT INTO settings (setting_key, setting_value) VALUES ('academic_year', 'next year')`)
	check("invalid")
	if n := k1Count(t, db, `SELECT COUNT(*) FROM expenses`); n != 0 {
		t.Errorf("%d expenses written without a valid academic year", n)
	}
	tchExpect(t, "categories need no year", c.do("GET", expCatPath, nil), 200, "")
	k1Exec(t, db, `UPDATE settings SET setting_value = '2026-2027' WHERE setting_key = 'academic_year'`)
	tchExpect(t, "restored", c.expense("category_id", rent, "amount", 1), 200, "Expense recorded")
}

// TestExpensesMigration runs 000036 down and up again (twice: idempotent, seeds not duplicated).
func TestExpensesMigration(t *testing.T) {
	db, _ := setupThrowawayDB(t, "expmigr")
	up, down := a14Migration(t, "000036_create_expenses.up.sql"), a14Migration(t, "000036_create_expenses.down.sql")
	state := func() string {
		tables := k1Count(t, db, `SELECT COUNT(*) FROM information_schema.tables WHERE table_name IN ('expenses', 'expense_categories')`)
		if tables < 2 {
			return fmt.Sprintf("tables=%d", tables)
		}
		return fmt.Sprintf("tables=%d categories=%d salary=%d", tables,
			k1Count(t, db, `SELECT COUNT(*) FROM expense_categories`), k1Count(t, db, `SELECT COUNT(*) FROM expense_categories WHERE is_salary`))
	}
	tuiWant(t, "after up", state(), "tables=2 categories=7 salary=1")
	k1Exec(t, db, down)
	tuiWant(t, "after down", state(), "tables=0")
	k1Exec(t, db, up, up)
	tuiWant(t, "after up again (twice)", state(), "tables=2 categories=7 salary=1")
}
