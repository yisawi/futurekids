package handlers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"future_kids/internal/tz"
)

// Tuition, in whole Iraqi dinars, per academic year (the 'academic_year' setting). A student's
// account for a year (tuition_accounts) freezes the grade, fee and discount: it is created on the
// first discount or payment, and for every active student whose grade has a fee when the academic
// year changes. Before that, the current year's figures follow the student's grade fee live, and
// other years count as "fee not set". Payments are never edited or deleted, only voided. The
// remaining balance is computed on read: fee - discount - the non-voided payments.

// AcademicYearSetting is the settings key of the current academic year, YYYY-YYYY.
const AcademicYearSetting = "academic_year"

// Tuition limits. Amounts are whole dinars.
const (
	MaxTuitionAmount = 1000000000
	MaxTuitionGrade  = 50
	MaxTuitionNote   = 200
)

// Tuition messages.
const (
	AcademicYearMessage      = "academic_year must be two consecutive years, for example 2026-2027"
	TuitionStudentIDMessage  = "student_id must be a positive student id"
	TuitionPaymentIDMessage  = "id must be a positive payment id"
	TuitionPaidAtMessage     = "paid_at must be formatted as YYYY-MM-DD"
	TuitionPaidAtFuture      = "paid_at cannot be in the future"
	TuitionHasRemainingError = "has_remaining must be true or false"
)

var academicYearRE = regexp.MustCompile(`^20[0-9]{2}-20[0-9]{2}$`)

// validAcademicYear reports whether y is two consecutive years, such as 2026-2027.
func validAcademicYear(y string) bool {
	if !academicYearRE.MatchString(y) {
		return false
	}
	first, _ := strconv.Atoi(y[:4])
	second, _ := strconv.Atoi(y[5:])
	return second == first+1
}

// tuitionRefusal is a write refused because of the request or the stored data; it is answered
// with its status and message, never as a 500.
type tuitionRefusal struct {
	status  int
	message string
}

func (e *tuitionRefusal) Error() string { return e.message }

var (
	errTuitionStudentNotFound       = &tuitionRefusal{http.StatusNotFound, "Student not found"}
	errTuitionPaymentNotFound       = &tuitionRefusal{http.StatusNotFound, "Payment not found"}
	errTuitionFeeNotSet             = &tuitionRefusal{http.StatusConflict, "Set the fee for this grade first"}
	errTuitionOverpay               = &tuitionRefusal{http.StatusConflict, "Payment exceeds the remaining balance"}
	errTuitionDiscountOverFee       = &tuitionRefusal{http.StatusConflict, "Discount exceeds the fee"}
	errTuitionDiscountOverRemaining = &tuitionRefusal{http.StatusConflict, "Discount exceeds the remaining balance"}
	errTuitionAlreadyVoided         = &tuitionRefusal{http.StatusConflict, "Payment is already voided"}
)

// errAcademicYearMissing means the academic_year setting row is missing or invalid; it is
// answered with a 500 and logged as an error, never replaced by a default.
var errAcademicYearMissing = errors.New("the academic_year setting is missing or invalid")

// respondTuitionError answers a failed tuition request.
func respondTuitionError(w http.ResponseWriter, op string, err error, attrs ...any) {
	var refusal *tuitionRefusal
	if errors.As(err, &refusal) {
		respondError(w, refusal.status, refusal.message)
		return
	}
	respondInternalError(w, "Internal server error", op, err, attrs...)
}

// currentAcademicYear reads the academic_year setting; lock is "" or a row-locking clause.
func currentAcademicYear(ctx context.Context, q rowQueryer, lock string) (string, error) {
	var year string
	err := q.QueryRowContext(ctx, `SELECT setting_value FROM settings WHERE setting_key = $1 `+lock, AcademicYearSetting).Scan(&year)
	if err == sql.ErrNoRows || (err == nil && !validAcademicYear(year)) {
		return "", errAcademicYearMissing
	}
	return year, err
}

// tuitionYear returns the requested academic year (the query parameter, or the body's value when
// requested is not nil), defaulting to the current one, and whether it is the current one. ok is
// false when an answer has been written.
func (app *AppEnv) tuitionYear(w http.ResponseWriter, r *http.Request, requested string) (year string, current, ok bool) {
	cur, err := currentAcademicYear(r.Context(), app.DB, "")
	if err != nil {
		respondTuitionError(w, "Tuition: reading the academic year failed", err)
		return "", false, false
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return cur, true, true
	}
	if !validAcademicYear(requested) {
		respondError(w, http.StatusBadRequest, AcademicYearMessage)
		return "", false, false
	}
	return requested, requested == cur, true
}

// TuitionRow is one student's figures for an academic year. fee and remaining are null, and
// fee_set false, when no fee applies.
type TuitionRow struct {
	StudentID int     `json:"student_id"`
	FullName  string  `json:"full_name"`
	Grade     *string `json:"grade"`
	Section   *string `json:"section"`
	IsActive  bool    `json:"is_active"`
	Fee       *int64  `json:"fee"`
	Discount  int64   `json:"discount"`
	Paid      int64   `json:"paid"`
	Remaining *int64  `json:"remaining"`
	FeeSet    bool    `json:"fee_set"`
}

// TuitionPayment is one payment, voided ones included.
type TuitionPayment struct {
	ID         int64   `json:"id"`
	Amount     int64   `json:"amount"`
	PaidAt     string  `json:"paid_at"`
	Note       *string `json:"note"`
	Status     string  `json:"status"`
	CreatedAt  string  `json:"created_at"`
	VoidedAt   *string `json:"voided_at"`
	VoidReason *string `json:"void_reason"`
}

// TuitionDetail is one student's figures for a year with the discount note and every payment,
// newest first.
type TuitionDetail struct {
	TuitionRow
	DiscountNote *string          `json:"discount_note"`
	Payments     []TuitionPayment `json:"payments"`
}

// TuitionSummary totals the listed rows; rows without a fee add only to not_set_count.
type TuitionSummary struct {
	TotalFee       int64 `json:"total_fee"`
	TotalDiscount  int64 `json:"total_discount"`
	TotalCollected int64 `json:"total_collected"`
	TotalRemaining int64 `json:"total_remaining"`
	NotSetCount    int   `json:"not_set_count"`
}

// tuitionRecord is a TuitionRow with what only some answers need.
type tuitionRecord struct {
	TuitionRow
	discountNote *string
	accountID    *int
	lastUpdated  *time.Time
}

// tuitionFilter selects the students of tuitionRows. Without studentID, the rows are the active
// students and every student with an account that year; parentID keeps that parent's active
// children only. nil fields do not filter.
type tuitionFilter struct {
	year      string
	current   bool
	studentID any
	parentID  any
	grade     any
	section   any
	name      any
}

// tuitionRows computes the figures of the students f selects, sorted by name. An account's frozen
// grade, fee and discount win; without one, the current year uses the grade's fee live.
func (app *AppEnv) tuitionRows(ctx context.Context, f tuitionFilter) ([]tuitionRecord, error) {
	rows, err := app.DB.QueryContext(ctx, `
		SELECT s.id, s.full_name, COALESCE(a.grade, NULLIF(btrim(s.grade), '')), s.section, COALESCE(s.is_active, false),
		       COALESCE(a.fee, f.fee), COALESCE(a.discount, 0), a.discount_note, COALESCE(p.paid, 0), a.id,
		       GREATEST(a.updated_at, p.last_at, f.updated_at)
		FROM students s
		LEFT JOIN tuition_accounts a ON a.student_id = s.id AND a.academic_year = $1
		LEFT JOIN tuition_fees f ON a.id IS NULL AND $2::bool AND f.academic_year = $1 AND f.grade = btrim(s.grade)
		LEFT JOIN LATERAL (
			SELECT SUM(amount) FILTER (WHERE voided_at IS NULL) AS paid, MAX(COALESCE(voided_at, created_at)) AS last_at
			FROM tuition_payments WHERE account_id = a.id
		) p ON true
		WHERE (CASE WHEN $3::int IS NULL THEN s.is_active = true OR a.id IS NOT NULL ELSE s.id = $3 END)
		  AND ($4::int IS NULL OR (s.parent_id = $4 AND s.is_active = true))
		  AND ($5::text IS NULL OR COALESCE(a.grade, btrim(s.grade)) = $5)
		  AND ($6::text IS NULL OR btrim(s.section) = $6)
		  AND ($7::text IS NULL OR s.full_name ILIKE $7)
		ORDER BY s.full_name, s.id`, f.year, f.current, f.studentID, f.parentID, f.grade, f.section, f.name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	records := []tuitionRecord{}
	for rows.Next() {
		var rec tuitionRecord
		if err := rows.Scan(&rec.StudentID, &rec.FullName, &rec.Grade, &rec.Section, &rec.IsActive, &rec.Fee, &rec.Discount,
			&rec.discountNote, &rec.Paid, &rec.accountID, &rec.lastUpdated); err != nil {
			return nil, err
		}
		if rec.Fee != nil {
			remaining := *rec.Fee - rec.Discount - rec.Paid
			rec.Remaining, rec.FeeSet = &remaining, true
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// tuitionDetail returns one student's figures and payments for a year.
func (app *AppEnv) tuitionDetail(ctx context.Context, studentID int, year string, current bool) (TuitionDetail, error) {
	records, err := app.tuitionRows(ctx, tuitionFilter{year: year, current: current, studentID: studentID})
	if err != nil {
		return TuitionDetail{}, err
	}
	if len(records) == 0 {
		return TuitionDetail{}, errTuitionStudentNotFound
	}
	d := TuitionDetail{TuitionRow: records[0].TuitionRow, DiscountNote: records[0].discountNote, Payments: []TuitionPayment{}}
	if records[0].accountID == nil {
		return d, nil
	}
	rows, err := app.DB.QueryContext(ctx, `
		SELECT id, amount, TO_CHAR(paid_at, 'YYYY-MM-DD'), note, created_at, voided_at, void_reason
		FROM tuition_payments WHERE account_id = $1 ORDER BY id DESC`, *records[0].accountID)
	if err != nil {
		return TuitionDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var p TuitionPayment
		var created time.Time
		var voided *time.Time
		if err := rows.Scan(&p.ID, &p.Amount, &p.PaidAt, &p.Note, &created, &voided, &p.VoidReason); err != nil {
			return TuitionDetail{}, err
		}
		p.Status, p.CreatedAt = "active", created.In(tz.Baghdad).Format(time.RFC3339)
		if voided != nil {
			s := voided.In(tz.Baghdad).Format(time.RFC3339)
			p.Status, p.VoidedAt = "voided", &s
		}
		d.Payments = append(d.Payments, p)
	}
	return d, rows.Err()
}

// AdminTuitionHandler (GET /api/admin/tuition) lists one row per active student, plus every
// inactive student with an account that year, for ?academic_year= (default the current one),
// filtered by ?grade=, ?section=, ?search= (part of the name) and ?has_remaining=true, with the
// totals of the listed rows.
func (app *AppEnv) AdminTuitionHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	year, current, ok := app.tuitionYear(w, r, q.Get("academic_year"))
	if !ok {
		return
	}
	f := tuitionFilter{year: year, current: current}
	if s := strings.TrimSpace(q.Get("grade")); s != "" {
		f.grade = s
	}
	if s := strings.TrimSpace(q.Get("section")); s != "" {
		f.section = s
	}
	if s := strings.TrimSpace(q.Get("search")); s != "" {
		f.name = likePattern(s)
	}
	var onlyRemaining bool
	switch q.Get("has_remaining") {
	case "", "false":
	case "true":
		onlyRemaining = true
	default:
		respondError(w, http.StatusBadRequest, TuitionHasRemainingError)
		return
	}
	records, err := app.tuitionRows(r.Context(), f)
	if err != nil {
		respondInternalError(w, "Database error", "AdminTuitionHandler: query failed", err, "academic_year", year)
		return
	}
	var sum TuitionSummary
	items := []TuitionRow{}
	for _, rec := range records {
		if onlyRemaining && (rec.Remaining == nil || *rec.Remaining <= 0) {
			continue
		}
		if rec.FeeSet {
			sum.TotalFee += *rec.Fee
			sum.TotalRemaining += *rec.Remaining
		} else {
			sum.NotSetCount++
		}
		sum.TotalDiscount += rec.Discount
		sum.TotalCollected += rec.Paid
		items = append(items, rec.TuitionRow)
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "academic_year": year, "summary": sum, "data": items})
}

// positiveQueryID parses a required positive integer query parameter; 0 when it is not one.
func positiveQueryID(r *http.Request, name string) int {
	if id, err := strconv.Atoi(r.URL.Query().Get(name)); err == nil && id >= 1 {
		return id
	}
	return 0
}

// AdminTuitionStudentHandler (GET /api/admin/tuition/student?student_id=&academic_year=) returns
// one student's figures for a year with every payment, voided ones included. Inactive students
// are allowed.
func (app *AppEnv) AdminTuitionStudentHandler(w http.ResponseWriter, r *http.Request) {
	studentID := positiveQueryID(r, "student_id")
	if studentID == 0 {
		respondError(w, http.StatusBadRequest, TuitionStudentIDMessage)
		return
	}
	year, current, ok := app.tuitionYear(w, r, r.URL.Query().Get("academic_year"))
	if !ok {
		return
	}
	d, err := app.tuitionDetail(r.Context(), studentID, year, current)
	if err != nil {
		respondTuitionError(w, "AdminTuitionStudentHandler: query failed", err, "student_id", studentID)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "academic_year": year, "data": d})
}

// TuitionFee is one grade's fee for the current academic year; fee is null when unset.
type TuitionFee struct {
	Grade     string  `json:"grade"`
	Fee       *int64  `json:"fee"`
	UpdatedAt *string `json:"updated_at"`
}

// TuitionFeeRequest is the body of PUT /api/admin/tuition/fees. apply_to_existing also re-prices
// every current-year account whose student is now in the grade.
type TuitionFeeRequest struct {
	Grade           string `json:"grade"`
	Fee             *int64 `json:"fee"`
	ApplyToExisting bool   `json:"apply_to_existing"`
}

// amountProblem checks a required amount between min and MaxTuitionAmount.
func amountProblem(field string, v *int64, min int64) string {
	switch {
	case v == nil:
		return field + " is required"
	case *v < min || *v > MaxTuitionAmount:
		return fmt.Sprintf("%s must be from %d to %d", field, min, MaxTuitionAmount)
	}
	return ""
}

// AdminTuitionFeesHandler serves /api/admin/tuition/fees for the current academic year: GET lists
// the grades of active students and every grade with a fee, PUT sets one grade's fee.
func (app *AppEnv) AdminTuitionFeesHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		year, _, ok := app.tuitionYear(w, r, "")
		if !ok {
			return
		}
		rows, err := app.DB.QueryContext(r.Context(), `
			SELECT g.grade, f.fee, f.updated_at
			FROM (
				SELECT btrim(s.grade) AS grade FROM students s WHERE s.is_active = true AND btrim(s.grade) <> ''
				UNION
				SELECT grade FROM tuition_fees WHERE academic_year = $1
			) g
			LEFT JOIN tuition_fees f ON f.academic_year = $1 AND f.grade = g.grade
			ORDER BY g.grade`, year)
		if err != nil {
			respondInternalError(w, "Database error", "AdminTuitionFeesHandler: query failed", err)
			return
		}
		defer rows.Close()
		fees := []TuitionFee{}
		for rows.Next() {
			var fee TuitionFee
			var updated *time.Time
			if err := rows.Scan(&fee.Grade, &fee.Fee, &updated); err != nil {
				respondInternalError(w, "Database error", "AdminTuitionFeesHandler: scan failed", err)
				return
			}
			if updated != nil {
				s := updated.In(tz.Baghdad).Format(time.RFC3339)
				fee.UpdatedAt = &s
			}
			fees = append(fees, fee)
		}
		if err := rows.Err(); err != nil {
			respondInternalError(w, "Database error", "AdminTuitionFeesHandler: rows iteration failed", err)
			return
		}
		respondJSON(w, http.StatusOK, map[string]any{"status": "success", "academic_year": year, "data": fees})
	case http.MethodPut:
		var req TuitionFeeRequest
		if !decodeJSONBody(w, r, &req, "Invalid request body") {
			return
		}
		req.Grade = strings.TrimSpace(req.Grade)
		if req.Grade == "" {
			respondError(w, http.StatusBadRequest, "grade is required")
			return
		}
		if msg := firstProblem(tooLong("grade", req.Grade, MaxTuitionGrade), amountProblem("fee", req.Fee, 1)); msg != "" {
			respondError(w, http.StatusBadRequest, msg)
			return
		}
		year, feeID, updated, err := app.setTuitionFee(r.Context(), req)
		if err != nil {
			respondTuitionError(w, "AdminTuitionFeesHandler: save failed", err)
			return
		}
		NoteActivity(r, "fee", strconv.Itoa(feeID), fmt.Sprintf("year=%s accounts=%d", year, updated))
		respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Fee saved", "data": map[string]any{
			"academic_year": year, "grade": req.Grade, "fee": *req.Fee, "updated_accounts": updated,
		}})
	default:
		respondError(w, http.StatusMethodNotAllowed, "Method not allowed")
	}
}

// MaxNegativeStudentIDs is how many student ids the apply_to_existing refusal names.
const MaxNegativeStudentIDs = 10

// setTuitionFee saves a grade's current-year fee and, with apply_to_existing, re-prices the
// current-year accounts of the students now in that grade, refusing (and changing nothing) when
// one of them would end with a negative remaining.
func (app *AppEnv) setTuitionFee(ctx context.Context, req TuitionFeeRequest) (year string, feeID int, updated int64, err error) {
	tx, err := app.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, 0, err
	}
	defer tx.Rollback()
	if year, err = currentAcademicYear(ctx, tx, "FOR SHARE"); err != nil {
		return "", 0, 0, err
	}
	if err = tx.QueryRowContext(ctx, `
		INSERT INTO tuition_fees (academic_year, grade, fee) VALUES ($1, $2, $3)
		ON CONFLICT (academic_year, grade) DO UPDATE SET fee = EXCLUDED.fee, updated_at = now()
		RETURNING id`, year, req.Grade, *req.Fee).Scan(&feeID); err != nil {
		return "", 0, 0, err
	}
	if req.ApplyToExisting {
		rows, err := tx.QueryContext(ctx, `
			SELECT a.id, a.student_id, a.discount
			FROM tuition_accounts a JOIN students s ON s.id = a.student_id
			WHERE a.academic_year = $1 AND btrim(s.grade) = $2
			ORDER BY a.student_id
			FOR UPDATE OF a`, year, req.Grade)
		if err != nil {
			return "", 0, 0, err
		}
		type account struct {
			id, studentID int
			discount      int64
		}
		var accounts []account
		for rows.Next() {
			var a account
			if err := rows.Scan(&a.id, &a.studentID, &a.discount); err != nil {
				rows.Close()
				return "", 0, 0, err
			}
			accounts = append(accounts, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", 0, 0, err
		}
		var negative []string
		for _, a := range accounts {
			var paid int64
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM tuition_payments WHERE account_id = $1 AND voided_at IS NULL`, a.id).Scan(&paid); err != nil {
				return "", 0, 0, err
			}
			if *req.Fee-a.discount-paid < 0 {
				negative = append(negative, strconv.Itoa(a.studentID))
			}
		}
		if len(negative) > 0 {
			shown := negative[:min(len(negative), MaxNegativeStudentIDs)]
			return "", 0, 0, &tuitionRefusal{http.StatusConflict, fmt.Sprintf(
				"apply_to_existing would make %d balances negative (student ids: %s)", len(negative), strings.Join(shown, ", "))}
		}
		res, err := tx.ExecContext(ctx, `
			UPDATE tuition_accounts a SET grade = $2, fee = $3, updated_at = now()
			FROM students s
			WHERE s.id = a.student_id AND a.academic_year = $1 AND btrim(s.grade) = $2`, year, req.Grade, *req.Fee)
		if err != nil {
			return "", 0, 0, err
		}
		if updated, err = res.RowsAffected(); err != nil {
			return "", 0, 0, err
		}
	}
	return year, feeID, updated, tx.Commit()
}

// lockTuitionAccount returns a student's account for a year, locked for the rest of tx, with the
// sum of its non-voided payments. In the current year a missing account is created from the
// grade's fee; it is refused when the grade has none, and in other years when none exists.
func lockTuitionAccount(ctx context.Context, tx *sql.Tx, studentID int, year string, current bool) (id int, fee, discount, paid int64, err error) {
	var exists bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM students WHERE id = $1)`, studentID).Scan(&exists); err != nil {
		return
	}
	if !exists {
		err = errTuitionStudentNotFound
		return
	}
	if current {
		if _, err = tx.ExecContext(ctx, `
			INSERT INTO tuition_accounts (student_id, academic_year, grade, fee)
			SELECT s.id, $2, f.grade, f.fee
			FROM students s JOIN tuition_fees f ON f.academic_year = $2 AND f.grade = btrim(s.grade)
			WHERE s.id = $1
			ON CONFLICT (student_id, academic_year) DO NOTHING`, studentID, year); err != nil {
			return
		}
	}
	err = tx.QueryRowContext(ctx, `
		SELECT id, fee, discount FROM tuition_accounts
		WHERE student_id = $1 AND academic_year = $2
		FOR UPDATE`, studentID, year).Scan(&id, &fee, &discount)
	if err == sql.ErrNoRows {
		err = errTuitionFeeNotSet
		return
	}
	if err != nil {
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(amount), 0) FROM tuition_payments WHERE account_id = $1 AND voided_at IS NULL`, id).Scan(&paid)
	return
}

// tuitionWrite runs write in a transaction that holds the academic year setting (so a year switch
// waits) and answers with the student's updated detail.
func (app *AppEnv) tuitionWrite(w http.ResponseWriter, r *http.Request, op, message string, studentID int, requestedYear string,
	write func(tx *sql.Tx, year string, current bool) (int, error)) {
	year, current, ok := app.tuitionYear(w, r, requestedYear)
	if !ok {
		return
	}
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		cur, err := currentAcademicYear(r.Context(), tx, "FOR SHARE")
		if err != nil {
			return err
		}
		current = year == cur
		if studentID, err = write(tx, year, current); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		respondTuitionError(w, op, err, "student_id", studentID)
		return
	}
	d, err := app.tuitionDetail(r.Context(), studentID, year, current)
	if err != nil {
		respondTuitionError(w, op, err, "student_id", studentID)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": message, "academic_year": year, "data": d})
}

// optionalNote trims an optional note (blank becomes null) and checks its length.
func optionalNote(field string, s *string) (*string, string) {
	s = blankToNil(s)
	if s == nil {
		return nil, ""
	}
	return s, tooLong(field, *s, MaxTuitionNote)
}

// TuitionDiscountRequest is the body of PUT /api/admin/tuition/discount, a full replacement:
// discount 0 without a note clears it.
type TuitionDiscountRequest struct {
	StudentID    int     `json:"student_id"`
	AcademicYear string  `json:"academic_year"`
	Discount     *int64  `json:"discount"`
	Note         *string `json:"note"`
}

// AdminTuitionDiscountHandler (PUT /api/admin/tuition/discount) sets a student's discount for a
// year (default the current one).
func (app *AppEnv) AdminTuitionDiscountHandler(w http.ResponseWriter, r *http.Request) {
	var req TuitionDiscountRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.StudentID < 1 {
		respondError(w, http.StatusBadRequest, TuitionStudentIDMessage)
		return
	}
	note, noteProblem := optionalNote("note", req.Note)
	if msg := firstProblem(amountProblem("discount", req.Discount, 0), noteProblem); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	app.tuitionWrite(w, r, "AdminTuitionDiscountHandler: save failed", "Discount saved", req.StudentID, req.AcademicYear,
		func(tx *sql.Tx, year string, current bool) (int, error) {
			NoteActivity(r, "student", strconv.Itoa(req.StudentID), "year="+year)
			id, fee, _, paid, err := lockTuitionAccount(r.Context(), tx, req.StudentID, year, current)
			if err != nil {
				return req.StudentID, err
			}
			switch {
			case *req.Discount > fee:
				return req.StudentID, errTuitionDiscountOverFee
			case *req.Discount+paid > fee:
				return req.StudentID, errTuitionDiscountOverRemaining
			}
			_, err = tx.ExecContext(r.Context(), `
				UPDATE tuition_accounts SET discount = $2, discount_note = $3, updated_at = now() WHERE id = $1`, id, *req.Discount, note)
			return req.StudentID, err
		})
}

// TuitionPaymentRequest is the body of POST /api/admin/tuition/payments.
type TuitionPaymentRequest struct {
	StudentID    int     `json:"student_id"`
	AcademicYear string  `json:"academic_year"`
	Amount       *int64  `json:"amount"`
	PaidAt       string  `json:"paid_at"`
	Note         *string `json:"note"`
}

// AdminTuitionPaymentsHandler (POST /api/admin/tuition/payments) records a cash payment for a
// year (default the current one), dated paid_at (default today, never in the future). A payment
// above the remaining balance is refused.
func (app *AppEnv) AdminTuitionPaymentsHandler(w http.ResponseWriter, r *http.Request) {
	var req TuitionPaymentRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.StudentID < 1 {
		respondError(w, http.StatusBadRequest, TuitionStudentIDMessage)
		return
	}
	note, noteProblem := optionalNote("note", req.Note)
	if msg := firstProblem(amountProblem("amount", req.Amount, 1), noteProblem); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	paidAt := strings.TrimSpace(req.PaidAt)
	if paidAt == "" {
		paidAt = today()
	}
	if t, err := time.Parse("2006-01-02", paidAt); err != nil || !yearInRange(t) {
		respondError(w, http.StatusBadRequest, TuitionPaidAtMessage)
		return
	}
	if paidAt > today() {
		respondError(w, http.StatusBadRequest, TuitionPaidAtFuture)
		return
	}
	app.tuitionWrite(w, r, "AdminTuitionPaymentsHandler: save failed", "Payment recorded", req.StudentID, req.AcademicYear,
		func(tx *sql.Tx, year string, current bool) (int, error) {
			id, fee, discount, paid, err := lockTuitionAccount(r.Context(), tx, req.StudentID, year, current)
			if err != nil {
				return req.StudentID, err
			}
			if *req.Amount > fee-discount-paid {
				return req.StudentID, errTuitionOverpay
			}
			var paymentID int64
			if err := tx.QueryRowContext(r.Context(), `
				INSERT INTO tuition_payments (account_id, amount, paid_at, note) VALUES ($1, $2, $3, $4) RETURNING id`,
				id, *req.Amount, paidAt, note).Scan(&paymentID); err != nil {
				return req.StudentID, err
			}
			NoteActivity(r, "payment", strconv.FormatInt(paymentID, 10), fmt.Sprintf("student=%d year=%s", req.StudentID, year))
			return req.StudentID, nil
		})
}

// TuitionVoidRequest is the body of POST /api/admin/tuition/payments/void.
type TuitionVoidRequest struct {
	ID     int64  `json:"id"`
	Reason string `json:"reason"`
}

// AdminTuitionVoidHandler (POST /api/admin/tuition/payments/void) voids a payment with a reason.
// It stays in the history and leaves every sum; a correct payment is recorded as a new one.
func (app *AppEnv) AdminTuitionVoidHandler(w http.ResponseWriter, r *http.Request) {
	var req TuitionVoidRequest
	if !decodeJSONBody(w, r, &req, "Invalid request body") {
		return
	}
	if req.ID < 1 {
		respondError(w, http.StatusBadRequest, TuitionPaymentIDMessage)
		return
	}
	NoteActivity(r, "payment", strconv.FormatInt(req.ID, 10), "")
	req.Reason = strings.TrimSpace(req.Reason)
	if req.Reason == "" {
		respondError(w, http.StatusBadRequest, "reason is required")
		return
	}
	if msg := tooLong("reason", req.Reason, MaxTuitionNote); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	var studentID int
	var year string
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var voided bool
		err = tx.QueryRowContext(r.Context(), `
			SELECT a.student_id, a.academic_year, p.voided_at IS NOT NULL
			FROM tuition_payments p JOIN tuition_accounts a ON a.id = p.account_id
			WHERE p.id = $1
			FOR UPDATE OF p`, req.ID).Scan(&studentID, &year, &voided)
		switch {
		case err == sql.ErrNoRows:
			return errTuitionPaymentNotFound
		case err != nil:
			return err
		case voided:
			return errTuitionAlreadyVoided
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE tuition_payments SET voided_at = now(), void_reason = $2 WHERE id = $1`, req.ID, req.Reason); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		respondTuitionError(w, "AdminTuitionVoidHandler: void failed", err, "payment_id", req.ID)
		return
	}
	NoteActivity(r, "payment", strconv.FormatInt(req.ID, 10), fmt.Sprintf("student=%d", studentID))
	cur, err := currentAcademicYear(r.Context(), app.DB, "")
	if err != nil {
		respondTuitionError(w, "AdminTuitionVoidHandler: reading the academic year failed", err)
		return
	}
	d, err := app.tuitionDetail(r.Context(), studentID, year, year == cur)
	if err != nil {
		respondTuitionError(w, "AdminTuitionVoidHandler: query failed", err, "student_id", studentID)
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "message": "Payment voided", "academic_year": year, "data": d})
}

// changeAcademicYear answers PUT /api/admin/settings for the academic_year key. Before switching,
// it creates, for every active student whose grade has a fee in the old year and who has no
// account yet, an account freezing that fee, so the old year's balances never change afterwards.
func (app *AppEnv) changeAcademicYear(w http.ResponseWriter, r *http.Request, value string) {
	year := strings.TrimSpace(value)
	if !validAcademicYear(year) {
		respondError(w, http.StatusBadRequest, AcademicYearMessage)
		return
	}
	var frozen int64
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		old, err := currentAcademicYear(r.Context(), tx, "FOR UPDATE")
		if err != nil || old == year {
			return err
		}
		res, err := tx.ExecContext(r.Context(), `
			INSERT INTO tuition_accounts (student_id, academic_year, grade, fee)
			SELECT s.id, $1, f.grade, f.fee
			FROM students s JOIN tuition_fees f ON f.academic_year = $1 AND f.grade = btrim(s.grade)
			WHERE s.is_active = true
			ON CONFLICT (student_id, academic_year) DO NOTHING`, old)
		if err != nil {
			return err
		}
		if frozen, err = res.RowsAffected(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(r.Context(), `
			UPDATE settings SET setting_value = $2, updated_at = CURRENT_TIMESTAMP WHERE setting_key = $1`, AcademicYearSetting, year); err != nil {
			return err
		}
		return tx.Commit()
	}()
	if err != nil {
		respondTuitionError(w, "AdminSettingsHandler: academic year change failed", err)
		return
	}
	NoteActivity(r, "setting", SettingKeysTarget(AcademicYearSetting), fmt.Sprintf("accounts=%d", frozen))
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "message": "Setting saved"})
}

// MobileTuitionItem is what a parent sees of one child's tuition: the remaining balance only.
type MobileTuitionItem struct {
	StudentID    int     `json:"student_id"`
	FullName     string  `json:"full_name"`
	AcademicYear string  `json:"academic_year"`
	Remaining    *int64  `json:"remaining"`
	FeeSet       bool    `json:"fee_set"`
	LastUpdated  *string `json:"last_updated"`
}

// MobileTuitionHandler (GET /api/mobile/tuition) returns, for each active child of the parent in
// the token, the current academic year's remaining balance and when it last changed. No fee,
// discount or payment is shown.
func (app *AppEnv) MobileTuitionHandler(w http.ResponseWriter, r *http.Request) {
	parentID, ok := r.Context().Value(ParentIDKey).(int)
	if !ok {
		respondError(w, http.StatusUnauthorized, "Unauthorized context")
		return
	}
	year, err := currentAcademicYear(r.Context(), app.DB, "")
	if err != nil {
		respondTuitionError(w, "MobileTuitionHandler: reading the academic year failed", err, "parent_id", parentID)
		return
	}
	records, err := app.tuitionRows(r.Context(), tuitionFilter{year: year, current: true, parentID: parentID})
	if err != nil {
		respondInternalError(w, "Database error", "MobileTuitionHandler: query failed", err, "parent_id", parentID)
		return
	}
	items := []MobileTuitionItem{}
	for _, rec := range records {
		it := MobileTuitionItem{StudentID: rec.StudentID, FullName: rec.FullName, AcademicYear: year, Remaining: rec.Remaining, FeeSet: rec.FeeSet}
		if rec.lastUpdated != nil {
			s := rec.lastUpdated.In(tz.Baghdad).Format(time.RFC3339)
			it.LastUpdated = &s
		}
		items = append(items, it)
	}
	respondJSON(w, http.StatusOK, map[string]any{"status": "success", "data": items})
}
