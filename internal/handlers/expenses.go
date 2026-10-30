package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/tz"

	"github.com/jackc/pgx/v5/pgconn"
)

// Expenses are what the school actually paid, in whole Iraqi dinars, per academic year (the
// 'academic_year' setting, read as tuition reads it). They are never edited or deleted: a wrong
// one is voided with a reason and leaves every sum. A salary is an expense in the salary category
// (is_salary), optionally linked to a teacher; teacher records hold no salary. The summary's
// difference is the tuition collected minus the expenses: cash-based, so never a profit.

// Expense limits, in characters.
const MaxExpenseCategoryName = 50

// Expense messages.
const (
	ExpenseCategoryIDMessage   = "category_id must be a positive category id"
	ExpenseTeacherIDMessage    = "teacher_id must be a positive teacher id"
	ExpenseIDMessage           = "id must be a positive expense id"
	ExpenseDateMessage         = "expense_date must be formatted as YYYY-MM-DD"
	ExpenseDateFutureMessage   = "expense_date cannot be in the future"
	expenseCategoryUniqueIndex = "expense_categories_name_key"
)

var (
	errExpenseCategoryNotFound = &tuitionRefusal{http.StatusNotFound, "Category not found"}
	errExpenseTeacherNotFound  = &tuitionRefusal{http.StatusNotFound, TeacherNotFoundMessage}
	errExpenseNotFound         = &tuitionRefusal{http.StatusNotFound, "Expense not found"}
	errExpenseNotSalary        = &tuitionRefusal{http.StatusConflict, "teacher_id is allowed only for the salary category"}
	errExpenseAlreadyVoided    = &tuitionRefusal{http.StatusConflict, "Expense is already voided"}
	errExpenseCategoryTaken    = &tuitionRefusal{http.StatusConflict, "name is already used by another category"}
)

// ExpenseCategory is a kind of expense. Exactly one, seeded, is the salary category.
type ExpenseCategory struct {
	ID       int    `json:"id"`
	Name     string `json:"name"`
	IsSalary bool   `json:"is_salary"`
}

// ExpenseCategoryRef names an expense's category.
type ExpenseCategoryRef struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// ExpenseTeacher names the teacher a salary was paid to.
type ExpenseTeacher struct {
	ID       int    `json:"id"`
	FullName string `json:"full_name"`
}

// Expense is one expense as the admin API returns it, voided ones included.
type Expense struct {
	ID           int64              `json:"id"`
	Category     ExpenseCategoryRef `json:"category"`
	Amount       int64              `json:"amount"`
	ExpenseDate  string             `json:"expense_date"`
	Note         *string            `json:"note"`
	Teacher      *ExpenseTeacher    `json:"teacher"`
	AcademicYear string             `json:"academic_year"`
	Status       string             `json:"status"`
	CreatedAt    string             `json:"created_at"`
	VoidedAt     *string            `json:"voided_at"`
	VoidReason   *string            `json:"void_reason"`
}

const expenseSelect = `
	SELECT e.id, c.id, c.name, e.amount, TO_CHAR(e.expense_date, 'YYYY-MM-DD'), e.note,
	       t.id, t.first_name, t.father_name, t.grandfather_name, e.academic_year, e.created_at, e.voided_at, e.void_reason
	FROM expenses e
	JOIN expense_categories c ON c.id = e.category_id
	LEFT JOIN teachers t ON t.id = e.teacher_id`

func scanExpense(row interface{ Scan(...any) error }) (Expense, error) {
	var e Expense
	var teacherID *int
	var first, father, grandfather *string
	var created time.Time
	var voided *time.Time
	if err := row.Scan(&e.ID, &e.Category.ID, &e.Category.Name, &e.Amount, &e.ExpenseDate, &e.Note,
		&teacherID, &first, &father, &grandfather, &e.AcademicYear, &created, &voided, &e.VoidReason); err != nil {
		return e, err
	}
	if teacherID != nil {
		e.Teacher = &ExpenseTeacher{ID: *teacherID, FullName: teacherFullName(*first, *father, *grandfather)}
	}
	e.Status, e.CreatedAt = "active", created.In(tz.Baghdad).Format(time.RFC3339)
	if voided != nil {
		s := voided.In(tz.Baghdad).Format(time.RFC3339)
		e.Status, e.VoidedAt = "voided", &s
	}
	return e, nil
}

func (app *AppEnv) readExpense(ctx context.Context, id int64) (Expense, error) {
	return scanExpense(app.DB.QueryRowContext(ctx, expenseSelect+` WHERE e.id = $1`, id))
}

// expenseYear returns the requested academic year, defaulting to the current one. ok is false
// when an answer (400, or 500 for a missing or invalid setting) has been written.
func (app *AppEnv) expenseYear(w http.ResponseWriter, r *http.Request, requested string) (year string, ok bool) {
	cur, err := currentAcademicYear(r.Context(), app.DB, "")
	if err != nil {
		respondTuitionError(w, "Expenses: reading the academic year failed", err)
		return "", false
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return cur, true
	}
	if !validAcademicYear(requested) {
		respondError(w, http.StatusBadRequest, AcademicYearMessage)
		return "", false
	}
	return requested, true
}

// optionalQueryID parses an optional positive integer query parameter: nil when absent, ok false
// (and 400 written with msg) when malformed.
func optionalQueryID(w http.ResponseWriter, r *http.Request, name, msg string) (id any, ok bool) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return nil, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		respondError(w, http.StatusBadRequest, msg)
		return nil, false
	}
	return n, true
}

// AdminExpenseCategoriesHandler serves /api/admin/expenses/categories: GET lists every category
// by id, POST adds one (name only; never a salary category). There is no rename or delete.
func (app *AppEnv) AdminExpenseCategoriesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rows, err := app.DB.QueryContext(r.Context(), `SELECT id, name, is_salary FROM expense_categories ORDER BY id`)
		if err != nil {
			respondInternalError(w, "Database error", "AdminExpenseCategoriesHandler: query failed", err)
			return
		}
		defer rows.Close()
		categories := []ExpenseCategory{}
		for rows.Next() {
			var c ExpenseCategory
			if err := rows.Scan(&c.ID, &c.Name, &c.IsSalary); err != nil {
				respondInternalError(w, "Database error", "AdminExpenseCategoriesHandler: scan failed", err)
				return
			}
			categories = append(categories, c)
		}
		if err := rows.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminExpenseCategoriesHandler: rows iteration failed", err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"status": "success", "data": categories})
	case http.MethodPost:
		var req struct {
			Name string `json:"name"`
		}
		if !decodeJSONBody(w, r, &req, "Invalid request body") {
			return
		}
		// Trimmed, with inner whitespace collapsed; the unique index ignores case.
		name := strings.Join(strings.Fields(req.Name), " ")
		if name == "" {
			respondError(w, http.StatusBadRequest, "name is required")
			return
		}
		if msg := tooLong("name", name, MaxExpenseCategoryName); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		c := ExpenseCategory{Name: name}
		err := app.DB.QueryRowContext(r.Context(), `INSERT INTO expense_categories (name) VALUES ($1) RETURNING id`, name).Scan(&c.ID)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == expenseCategoryUniqueIndex {
			err = errExpenseCategoryTaken
		}
		if err != nil {
			respondTuitionError(w, "AdminExpenseCategoriesHandler: insert failed", err)
			return
		}
		NoteActivity(r, "category", strconv.Itoa(c.ID), "")
		respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Category added", "data": c})
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// ExpenseRequest is the body of POST /api/admin/expenses.
type ExpenseRequest struct {
	CategoryID   int     `json:"category_id"`
	Amount       *int64  `json:"amount"`
	ExpenseDate  string  `json:"expense_date"`
	Note         *string `json:"note"`
	TeacherID    *int    `json:"teacher_id"`
	AcademicYear string  `json:"academic_year"`
}

// AdminExpensesHandler serves /api/admin/expenses: GET lists a year's expenses, newest first,
// filtered by ?category_id=, ?month=YYYY-MM and ?teacher_id=, with the total of the non-voided
// listed rows; POST records an expense. teacher_id is allowed only in the salary category.
func (app *AppEnv) AdminExpensesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		app.listExpenses(w, r)
	case http.MethodPost:
		app.createExpense(w, r)
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

func (app *AppEnv) listExpenses(w http.ResponseWriter, r *http.Request) {
	year, ok := app.expenseYear(w, r, r.URL.Query().Get("academic_year"))
	if !ok {
		return
	}
	categoryID, ok := optionalQueryID(w, r, "category_id", ExpenseCategoryIDMessage)
	if !ok {
		return
	}
	teacherID, ok := optionalQueryID(w, r, "teacher_id", ExpenseTeacherIDMessage)
	if !ok {
		return
	}
	var month any
	if r.URL.Query().Get("month") != "" {
		m, ok := requestedMonth(w, r, Clock())
		if !ok {
			return
		}
		month = m
	}
	rows, err := app.DB.QueryContext(r.Context(), expenseSelect+`
		WHERE e.academic_year = $1
		  AND ($2::int IS NULL OR e.category_id = $2)
		  AND ($3::int IS NULL OR e.teacher_id = $3)
		  AND ($4::text IS NULL OR TO_CHAR(e.expense_date, 'YYYY-MM') = $4)
		ORDER BY e.expense_date DESC, e.id DESC`, year, categoryID, teacherID, month)
	if err != nil {
		respondInternalError(w, "Database error", "AdminExpensesHandler: list failed", err, "academic_year", year)
		return
	}
	defer rows.Close()
	expenses := []Expense{}
	var total int64
	for rows.Next() {
		e, err := scanExpense(rows)
		if err != nil {
			respondInternalError(w, "Database error", "AdminExpensesHandler: scan failed", err)
			return
		}
		if e.Status == "active" {
			total += e.Amount
		}
		expenses = append(expenses, e)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminExpensesHandler: rows iteration failed", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "academic_year": year, "summary": map[string]int64{"total": total}, "data": expenses})
}

func (app *AppEnv) createExpense(w http.ResponseWriter, r *http.Request) {
	var req ExpenseRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.CategoryID < 1 {
		respondError(w, http.StatusBadRequest, ExpenseCategoryIDMessage)
		return
	}
	if req.TeacherID != nil && *req.TeacherID < 1 {
		respondError(w, http.StatusBadRequest, ExpenseTeacherIDMessage)
		return
	}
	note, noteProblem := optionalNote("note", req.Note)
	if msg := firstProblem(amountProblem("amount", req.Amount, 1), noteProblem); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	date := strings.TrimSpace(req.ExpenseDate)
	if date == "" {
		date = today()
	}
	if t, err := time.Parse("2006-01-02", date); err != nil || !yearInRange(t) {
		respondError(w, http.StatusBadRequest, ExpenseDateMessage)
		return
	}
	if date > today() {
		respondError(w, http.StatusBadRequest, ExpenseDateFutureMessage)
		return
	}
	requested := strings.TrimSpace(req.AcademicYear)
	if requested != "" && !validAcademicYear(requested) {
		respondError(w, http.StatusBadRequest, AcademicYearMessage)
		return
	}
	var id int64
	var year string
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if year, err = currentAcademicYear(r.Context(), tx, "FOR SHARE"); err != nil {
			return err
		}
		if requested != "" {
			year = requested
		}
		var salary bool
		switch err := tx.QueryRowContext(r.Context(), `SELECT is_salary FROM expense_categories WHERE id = $1`, req.CategoryID).Scan(&salary); {
		case err == sql.ErrNoRows:
			return errExpenseCategoryNotFound
		case err != nil:
			return err
		}
		if req.TeacherID != nil {
			if !salary {
				return errExpenseNotSalary
			}
			var exists bool
			if err := tx.QueryRowContext(r.Context(), `SELECT EXISTS (SELECT 1 FROM teachers WHERE id = $1)`, *req.TeacherID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return errExpenseTeacherNotFound
			}
		}
		if err := tx.QueryRowContext(r.Context(), `
			INSERT INTO expenses (category_id, academic_year, amount, expense_date, note, teacher_id)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			req.CategoryID, year, *req.Amount, date, note, req.TeacherID).Scan(&id); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		respondTuitionError(w, "AdminExpensesHandler: create failed", err, "category_id", req.CategoryID)
		return
	}
	NoteActivity(r, "expense", strconv.FormatInt(id, 10), fmt.Sprintf("category=%d year=%s", req.CategoryID, year))
	e, err := app.readExpense(r.Context(), id)
	if err != nil {
		respondInternalError(w, "Database error", "AdminExpensesHandler: read back failed", err, "expense_id", id)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Expense recorded", "data": e})
}

// ExpenseVoidRequest is the body of POST /api/admin/expenses/void.
type ExpenseVoidRequest struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// AdminExpenseVoidHandler (POST /api/admin/expenses/void) voids an expense with a reason. It stays
// in the history and leaves every sum; a correct expense is recorded as a new one.
func (app *AppEnv) AdminExpenseVoidHandler(w http.ResponseWriter, r *http.Request) {
	var req ExpenseVoidRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.ID < 1 {
		respondError(w, http.StatusBadRequest, ExpenseIDMessage)
		return
	}
	NoteActivity(r, "expense", strconv.FormatInt(req.ID, 10), "")
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		respondError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if msg := tooLong("reason", req.Reason, MaxTuitionNote); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var voided bool
		switch err := tx.QueryRowContext(r.Context(), `SELECT voided_at IS NOT NULL FROM expenses WHERE id = $1 FOR UPDATE`, req.ID).Scan(&voided); {
		case err == sql.ErrNoRows:
			return errExpenseNotFound
		case err != nil:
			return err
		case voided:
			return errExpenseAlreadyVoided
		}
		if _, err := tx.ExecContext(r.Context(), `UPDATE expenses SET voided_at = now(), void_reason = $2 WHERE id = $1`, req.ID, req.Reason); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		respondTuitionError(w, "AdminExpenseVoidHandler: void failed", err, "expense_id", req.ID)
		return
	}
	e, err := app.readExpense(r.Context(), req.ID)
	if err != nil {
		respondInternalError(w, "Database error", "AdminExpenseVoidHandler: read back failed", err, "expense_id", req.ID)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Expense voided", "data": e})
}

// ExpenseTotals are a year's (or a month's) figures. collected and difference are null while a
// category filter is applied: next to one category's expenses they would mislead.
type ExpenseTotals struct {
	Expenses   int64  `json:"expenses"`
	Collected  *int64 `json:"collected"`
	Difference *int64 `json:"difference"`
}

// ExpenseMonth is one month of the summary.
type ExpenseMonth struct {
	Month string `json:"month"`
	ExpenseTotals
}

// ExpenseCategoryTotal is one category's non-voided total for the year.
type ExpenseCategoryTotal struct {
	CategoryID int    `json:"category_id"`
	Name       string `json:"name"`
	Total      int64  `json:"total"`
}

// AdminExpensesSummaryHandler (GET /api/admin/expenses/summary?academic_year=&category_id=)
// reports a year: total expenses, tuition collected (non-voided payments of that year's
// accounts, students who left included) and their difference; the total of every category; and
// every month with expenses or collections, ascending. With category_id, the expenses figures
// cover that category only, and collected and difference are null.
func (app *AppEnv) AdminExpensesSummaryHandler(w http.ResponseWriter, r *http.Request) {
	year, ok := app.expenseYear(w, r, r.URL.Query().Get("academic_year"))
	if !ok {
		return
	}
	categoryID, ok := optionalQueryID(w, r, "category_id", ExpenseCategoryIDMessage)
	if !ok {
		return
	}
	filtered := categoryID != nil
	ctx := r.Context()
	fail := func(err error) {
		respondTuitionError(w, "AdminExpensesSummaryHandler: query failed", err, "academic_year", year)
	}
	if filtered {
		var exists bool
		if err := app.DB.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM expense_categories WHERE id = $1)`, categoryID).Scan(&exists); err != nil {
			fail(err)
			return
		}
		if !exists {
			fail(errExpenseCategoryNotFound)
			return
		}
	}

	byCategory := []ExpenseCategoryTotal{}
	rows, err := app.DB.QueryContext(ctx, `
		SELECT c.id, c.name, COALESCE(SUM(e.amount) FILTER (WHERE e.voided_at IS NULL), 0)
		FROM expense_categories c
		LEFT JOIN expenses e ON e.category_id = c.id AND e.academic_year = $1
		WHERE $2::int IS NULL OR c.id = $2
		GROUP BY c.id
		ORDER BY c.id`, year, categoryID)
	if err != nil {
		fail(err)
		return
	}
	var totals ExpenseTotals
	for rows.Next() {
		var c ExpenseCategoryTotal
		if err := rows.Scan(&c.CategoryID, &c.Name, &c.Total); err != nil {
			rows.Close()
			fail(err)
			return
		}
		totals.Expenses += c.Total
		byCategory = append(byCategory, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fail(err)
		return
	}

	months := map[string]*ExpenseMonth{}
	month := func(m string) *ExpenseMonth {
		if months[m] == nil {
			months[m] = &ExpenseMonth{Month: m}
		}
		return months[m]
	}
	sums := func(query string, add func(*ExpenseMonth, int64), args ...any) error {
		rows, err := app.DB.QueryContext(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			var amount int64
			if err := rows.Scan(&m, &amount); err != nil {
				return err
			}
			add(month(m), amount)
		}
		return rows.Err()
	}
	if err := sums(`
		SELECT TO_CHAR(expense_date, 'YYYY-MM'), SUM(amount)
		FROM expenses
		WHERE academic_year = $1 AND voided_at IS NULL AND ($2::int IS NULL OR category_id = $2)
		GROUP BY 1`, func(m *ExpenseMonth, v int64) { m.Expenses = v }, year, categoryID); err != nil {
		fail(err)
		return
	}
	if !filtered {
		var collected int64
		if err := sums(`
			SELECT TO_CHAR(p.paid_at, 'YYYY-MM'), SUM(p.amount)
			FROM tuition_payments p JOIN tuition_accounts a ON a.id = p.account_id
			WHERE a.academic_year = $1 AND p.voided_at IS NULL
			GROUP BY 1`, func(m *ExpenseMonth, v int64) { m.Collected = &v; collected += v }, year); err != nil {
			fail(err)
			return
		}
		diff := collected - totals.Expenses
		totals.Collected, totals.Difference = &collected, &diff
	}

	byMonth := []ExpenseMonth{}
	for _, m := range months {
		if !filtered {
			if m.Collected == nil {
				zero := int64(0)
				m.Collected = &zero
			}
			diff := *m.Collected - m.Expenses
			m.Difference = &diff
		}
		byMonth = append(byMonth, *m)
	}
	sort.Slice(byMonth, func(i, j int) bool { return byMonth[i].Month < byMonth[j].Month })
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "academic_year": year, "category_id": categoryID,
		"totals": totals, "by_category": byCategory, "by_month": byMonth})
}
