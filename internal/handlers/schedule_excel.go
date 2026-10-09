package handlers

import (
	"archive/zip"
	"bytes"
	"fmt"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"future_kids/internal/tz"

	excelize "github.com/xuri/excelize/v2"
)

// Schedule import limits. The file may unpack to at most MaxScheduleUnzippedBytes in total, as
// declared by the zip, which is checked before the workbook is parsed in memory.
const (
	MaxScheduleFileBytes       int64 = 2 << 20
	MaxScheduleImportBodyBytes       = MaxScheduleFileBytes + 64<<10
	MaxScheduleUnzippedBytes   int64 = 20 << 20
	MaxScheduleImportRows            = 2000
	MaxScheduleImportClasses         = 100
	MaxScheduleImportErrors          = 50
	scheduleHeaderSearchRows         = 10
	scheduleHeaderMinMatches         = 3
	maxScheduleClassPartBytes  int64 = 200
)

// The columns of the schedule workbook, in export order, with the header names the import accepts.
const (
	colGrade = iota
	colSection
	colDay
	colPeriod
	colSubject
	colTeacher
)

var scheduleColumns = []struct {
	name    string
	aliases []string
}{
	{"المرحلة", []string{"المرحلة", "الصف", "grade"}},
	{"الشعبة", []string{"الشعبة", "section"}},
	{"اليوم", []string{"اليوم", "day"}},
	{"الحصة", []string{"الحصة", "period"}},
	{"المادة", []string{"المادة", "subject"}},
	{"المعلم", []string{"المعلم", "المدرس", "teacher"}},
}

var scheduleHeaderAliases = func() map[string]int {
	m := map[string]int{}
	for i, c := range scheduleColumns {
		for _, a := range c.aliases {
			m[normalizeArabic(a)] = i
		}
	}
	return m
}()

func scheduleHeaders() []string {
	out := make([]string, len(scheduleColumns))
	for i, c := range scheduleColumns {
		out[i] = c.name
	}
	return out
}

// scheduleClassesSQL lists every known class: the grade and section of active students (both
// non-blank) and every class with stored periods.
const scheduleClassesSQL = `
	SELECT grade, section FROM students
	WHERE is_active = true AND btrim(COALESCE(grade, '')) <> '' AND btrim(COALESCE(section, '')) <> ''
	UNION
	SELECT grade, section FROM weekly_schedules`

// ScheduleClass is one known class with its counts.
type ScheduleClass struct {
	Grade        string `json:"grade"`
	Section      string `json:"section"`
	StudentCount int    `json:"student_count"`
	PeriodCount  int    `json:"period_count"`
	DayCount     int    `json:"day_count"`
}

func (app *AppEnv) AdminScheduleClassesHandler(w http.ResponseWriter, r *http.Request) {
	rows, err := app.DB.QueryContext(r.Context(), `
		WITH classes AS (`+scheduleClassesSQL+`)
		SELECT c.grade, c.section,
			(SELECT COUNT(*) FROM students s WHERE s.is_active = true AND s.grade = c.grade AND s.section = c.section),
			(SELECT COUNT(*) FROM weekly_schedules ws WHERE ws.grade = c.grade AND ws.section = c.section),
			(SELECT COUNT(DISTINCT CASE WHEN day.rank = 8 THEN ws.day_of_week ELSE '#' || day.rank END)
				FROM weekly_schedules ws
				CROSS JOIN LATERAL (SELECT `+weekdayRankSQL+` AS rank) day
				WHERE ws.grade = c.grade AND ws.section = c.section)
		FROM classes c
		ORDER BY c.grade COLLATE "C", c.section COLLATE "C"`)
	if err != nil {
		respondInternalError(w, "Database error", "AdminScheduleClassesHandler: query failed", err)
		return
	}
	defer rows.Close()
	classes := []ScheduleClass{}
	for rows.Next() {
		var c ScheduleClass
		if err := rows.Scan(&c.Grade, &c.Section, &c.StudentCount, &c.PeriodCount, &c.DayCount); err != nil {
			respondInternalError(w, "Database error", "AdminScheduleClassesHandler: scan failed", err)
			return
		}
		classes = append(classes, c)
	}
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminScheduleClassesHandler: rows iteration failed", err)
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"status": "success", "data": classes})
}

// unsafeFileNameRune reports characters replaced by "_" in a download file name: path separators,
// quotes, spaces, controls and anything else that could break the header or a file system.
func unsafeFileNameRune(r rune) bool {
	return unicode.IsControl(r) || unicode.IsSpace(r) || !unicode.IsPrint(r) || strings.ContainsRune(`/\"'*:?<>|;,%`, r)
}

// scheduleFileDisposition is the Content-Disposition of one class's export: an attachment named
// schedule_<grade>_<section>_<date>.xlsx, with filename* (RFC 2231) when the name is not ASCII.
func scheduleFileDisposition(grade, section, date string) string {
	safe := func(s string) string {
		return strings.Map(func(r rune) rune {
			if unsafeFileNameRune(r) {
				return '_'
			}
			return r
		}, s)
	}
	name := fmt.Sprintf("schedule_%s_%s_%s.xlsx", safe(grade), safe(section), date)
	if v := mime.FormatMediaType("attachment", map[string]string{"filename": name}); v != "" {
		return v
	}
	return "attachment; filename=schedule_" + date + ".xlsx"
}

func (app *AppEnv) AdminScheduleExportHandler(w http.ResponseWriter, r *http.Request) {
	grade := strings.TrimSpace(r.URL.Query().Get("grade"))
	section := strings.TrimSpace(r.URL.Query().Get("section"))
	if msg := scheduleClassProblem(grade, section); msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}

	rows, err := app.DB.QueryContext(r.Context(), fmt.Sprintf(`
		WITH classes AS (`+scheduleClassesSQL+`),
		stored AS (
			SELECT DISTINCT ON (day.rank, ws.period_number) day.rank, ws.period_number, ws.subject_name, ws.teacher_name
			FROM weekly_schedules ws
			CROSS JOIN LATERAL (SELECT `+weekdayRankSQL+` AS rank) day
			WHERE ws.grade = $1 AND ws.section = $2 AND day.rank BETWEEN 1 AND 5 AND ws.period_number BETWEEN 1 AND %d
			ORDER BY day.rank, ws.period_number, ws.id
		)
		SELECT d.rank, p.n, COALESCE(st.subject_name, ''), COALESCE(st.teacher_name, '')
		FROM (SELECT 1 FROM classes WHERE grade = $1 AND section = $2 LIMIT 1) known
		CROSS JOIN generate_series(1, 5) AS d(rank)
		CROSS JOIN generate_series(1, %d) AS p(n)
		LEFT JOIN stored st ON st.rank = d.rank AND st.period_number = p.n
		ORDER BY d.rank, p.n`, MaxPeriodNumber, MaxPeriodNumber), grade, section)
	if err != nil {
		respondInternalError(w, "Database error", "AdminScheduleExportHandler: query failed", err, "grade", grade, "section", section)
		return
	}
	type gridRow struct {
		rank, period     int
		subject, teacher string
	}
	var grid []gridRow
	for rows.Next() {
		var g gridRow
		if err := rows.Scan(&g.rank, &g.period, &g.subject, &g.teacher); err != nil {
			rows.Close()
			respondInternalError(w, "Database error", "AdminScheduleExportHandler: scan failed", err, "grade", grade, "section", section)
			return
		}
		grid = append(grid, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		respondInternalError(w, "Database error", "AdminScheduleExportHandler: rows iteration failed", err, "grade", grade, "section", section)
		return
	}
	if len(grid) == 0 {
		respondError(w, http.StatusNotFound, "No class has this grade and section: no active student and no schedule")
		return
	}

	schoolName, err := app.readSchoolName(r.Context(), "AdminScheduleExportHandler")
	if err != nil {
		respondInternalError(w, "Database error", "AdminScheduleExportHandler: school name query failed", err, "grade", grade, "section", section)
		return
	}

	f, sheet := newReportWorkbook(schoolName, fmt.Sprintf("الجدول الأسبوعي - %s %s", grade, section), scheduleHeaders())
	defer f.Close()
	for i, g := range grid {
		for col, v := range []string{grade, section, schoolWeek[g.rank-1], strconv.Itoa(g.period), g.subject, g.teacher} {
			cell, _ := excelize.CoordinatesToCellName(col+1, reportFirstDataRow+i)
			f.SetCellStr(sheet, cell, v)
		}
	}

	respondXLSX(w, f, scheduleFileDisposition(grade, section, tz.Today()), "AdminScheduleExportHandler", "grade", grade, "section", section)
}

// ScheduleImportError is one problem in an uploaded schedule workbook: the worksheet, the Excel
// row number and the column's standard header name. The last item of a capped list has only a
// message saying how many more problems there are.
type ScheduleImportError struct {
	Sheet   string `json:"sheet,omitempty"`
	Row     int    `json:"row,omitempty"`
	Column  string `json:"column,omitempty"`
	Message string `json:"message"`

	sheetIndex int
	column     int
}

// ScheduleImportClass is one class replaced by an import.
type ScheduleImportClass struct {
	Grade           string `json:"grade"`
	Section         string `json:"section"`
	Periods         int    `json:"periods"`
	MatchedStudents int    `json:"matched_students"`
}

type scheduleClassKey struct{ grade, section string }

type importedRow struct {
	sheet      string
	sheetIndex int
	row        int
	class      scheduleClassKey
	period     SchedulePeriod
}

// scheduleImport is a parsed workbook: the periods of every class in the file, or its problems.
type scheduleImport struct {
	classes  map[scheduleClassKey][]SchedulePeriod
	problems []ScheduleImportError
}

const notAWorkbook = "file must be an Excel workbook (.xlsx)"

// readScheduleWorkbook parses and validates an uploaded workbook. A non-empty message is a
// problem with the file as a whole; otherwise problems lists every problem in its rows. When only
// is set, every row must belong to that class and the file must have at least one.
func readScheduleWorkbook(data []byte, only *scheduleClassKey) (imp *scheduleImport, message string) {
	if len(data) == 0 {
		return nil, "file is empty"
	}
	if !bytes.HasPrefix(data, []byte("PK\x03\x04")) {
		return nil, notAWorkbook
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, notAWorkbook
	}
	var unzipped uint64
	for _, zf := range zr.File {
		unzipped += zf.UncompressedSize64
		if zf.UncompressedSize64 > uint64(MaxScheduleUnzippedBytes) || unzipped > uint64(MaxScheduleUnzippedBytes) {
			return nil, "file must unpack to at most 20 MiB"
		}
	}
	defer func() {
		if p := recover(); p != nil {
			slog.Warn("Schedule import: the workbook could not be read", "panic", fmt.Sprint(p))
			imp, message = nil, notAWorkbook
		}
	}()
	f, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{UnzipSizeLimit: MaxScheduleUnzippedBytes, UnzipXMLSizeLimit: MaxScheduleUnzippedBytes})
	if err != nil {
		return nil, notAWorkbook
	}
	defer f.Close()
	sheets := f.GetSheetList()
	if len(sheets) == 0 {
		return nil, notAWorkbook
	}

	imp = &scheduleImport{classes: map[scheduleClassKey][]SchedulePeriod{}}
	problem := func(sheetIndex, row, column int, msg string) {
		imp.problems = append(imp.problems, ScheduleImportError{Sheet: sheets[sheetIndex], Row: row, Column: scheduleColumns[column].name, Message: msg, sheetIndex: sheetIndex, column: column})
	}
	var valid []importedRow
	dataRows, withHeader := 0, 0
	for si, name := range sheets {
		rows, headerFound, msg := readScheduleSheet(f, name, &dataRows, func(row, column int, msg string) { problem(si, row, column, msg) })
		if msg != "" {
			return nil, msg
		}
		if headerFound {
			withHeader++
		}
		for _, r := range rows {
			r.sheet, r.sheetIndex = name, si
			valid = append(valid, r)
		}
	}
	if withHeader == 0 {
		return nil, fmt.Sprintf("No worksheet has the header row (%s) in its first %d rows", strings.Join(scheduleHeaders(), ", "), scheduleHeaderSearchRows)
	}

	type dayKey struct {
		class scheduleClassKey
		day   string
	}
	firstOf := map[dayKey]importedRow{}
	seen := map[string]importedRow{}
	numbers := map[dayKey][]int{}
	var classOrder []scheduleClassKey
	for _, r := range valid {
		if only != nil && r.class != *only {
			column := colGrade
			if r.class.grade == only.grade {
				column = colSection
			}
			problem(r.sheetIndex, r.row, column, fmt.Sprintf("this file is for grade %s, section %s only", only.grade, only.section))
			continue
		}
		if _, ok := imp.classes[r.class]; !ok {
			classOrder = append(classOrder, r.class)
			imp.classes[r.class] = nil
		}
		dk := dayKey{r.class, r.period.DayOfWeek}
		key := fmt.Sprintf("%s\x00%s\x00%s\x00%d", r.class.grade, r.class.section, r.period.DayOfWeek, r.period.PeriodNumber)
		if first, dup := seen[key]; dup {
			problem(r.sheetIndex, r.row, colPeriod, fmt.Sprintf("period %d on %s for grade %s, section %s is already in sheet %s row %d", r.period.PeriodNumber, r.period.DayOfWeek, r.class.grade, r.class.section, first.sheet, first.row))
			continue
		}
		seen[key] = r
		if _, ok := firstOf[dk]; !ok {
			firstOf[dk] = r
		}
		numbers[dk] = append(numbers[dk], r.period.PeriodNumber)
		imp.classes[r.class] = append(imp.classes[r.class], r.period)
	}
	if only != nil && len(classOrder) == 0 && len(imp.problems) == 0 {
		return nil, fmt.Sprintf("The file has no rows for grade %s, section %s", only.grade, only.section)
	}
	if len(classOrder) > MaxScheduleImportClasses {
		return nil, fmt.Sprintf("The file has %d classes; at most %d can be imported at once", len(classOrder), MaxScheduleImportClasses)
	}
	for _, c := range classOrder {
		for _, day := range schoolWeek {
			dk := dayKey{c, day}
			if got, ok := numbers[dk]; ok {
				if p := dayPeriodsProblem(got); p != "" {
					r := firstOf[dk]
					problem(r.sheetIndex, r.row, colPeriod, fmt.Sprintf("grade %s, section %s has %s on %s; %s", c.grade, c.section, p, day, dayPeriodsRule))
				}
			}
		}
		sort.SliceStable(imp.classes[c], func(i, j int) bool {
			a, b := imp.classes[c][i], imp.classes[c][j]
			if a.DayOfWeek != b.DayOfWeek {
				return dayIndex(a.DayOfWeek) < dayIndex(b.DayOfWeek)
			}
			return a.PeriodNumber < b.PeriodNumber
		})
	}
	sort.SliceStable(imp.problems, func(i, j int) bool {
		a, b := imp.problems[i], imp.problems[j]
		if a.sheetIndex != b.sheetIndex {
			return a.sheetIndex < b.sheetIndex
		}
		if a.Row != b.Row {
			return a.Row < b.Row
		}
		return a.column < b.column
	})
	return imp, ""
}

func dayIndex(day string) int {
	for i, d := range schoolWeek {
		if d == day {
			return i
		}
	}
	return len(schoolWeek)
}

type rawScheduleRow struct {
	row     int
	values  [6]string
	present [6]bool
}

// readScheduleSheet reads one worksheet. headerFound is false when none of its first rows is the
// header row; the sheet is then ignored. dataRows counts the non-blank rows of every sheet read
// so far; a message is returned as soon as it passes MaxScheduleImportRows.
func readScheduleSheet(f *excelize.File, sheet string, dataRows *int, problem func(row, column int, msg string)) (valid []importedRow, headerFound bool, message string) {
	rows, err := f.Rows(sheet)
	if err != nil {
		return nil, false, notAWorkbook
	}
	var cols []int
	var raw []rawScheduleRow
	var formulaCells [][2]int
	for rowNum := 1; rows.Next(); rowNum++ {
		cells, err := rows.Columns(excelize.Options{RawCellValue: true})
		if err != nil {
			rows.Close()
			return nil, false, notAWorkbook
		}
		if cols == nil {
			if rowNum > scheduleHeaderSearchRows {
				break
			}
			found, ok := matchScheduleHeader(cells, rowNum, problem)
			if !ok {
				continue
			}
			headerFound = true
			if found == nil {
				break
			}
			cols = found
			continue
		}
		var r rawScheduleRow
		r.row = rowNum
		for i, c := range cols {
			if c < len(cells) {
				r.present[i] = true
				r.values[i] = strings.TrimSpace(cells[c])
				if cells[c] == "" {
					formulaCells = append(formulaCells, [2]int{rowNum, c})
				}
			}
		}
		if r.values[colSubject] == "" && r.values[colTeacher] == "" {
			continue
		}
		if *dataRows++; *dataRows > MaxScheduleImportRows {
			rows.Close()
			return nil, false, fmt.Sprintf("The file has more than %d schedule rows", MaxScheduleImportRows)
		}
		raw = append(raw, r)
	}
	if err := rows.Close(); err != nil {
		return nil, false, notAWorkbook
	}

	columnOf := map[int]int{}
	for i, c := range cols {
		columnOf[c] = i
	}
	for _, fc := range formulaCells {
		ref, _ := excelize.CoordinatesToCellName(fc[1]+1, fc[0])
		if formula, _ := f.GetCellFormula(sheet, ref); formula != "" {
			problem(fc[0], columnOf[fc[1]], fmt.Sprintf("cell %s has a formula without a saved value; type the value, or open and save the file in Excel", ref))
		}
	}

	for _, r := range raw {
		for _, col := range []int{colGrade, colSection} {
			v := r.values[col]
			if !strings.ContainsAny(v, ".eE") {
				continue
			}
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				continue
			}
			ref, _ := excelize.CoordinatesToCellName(cols[col]+1, r.row)
			if t, _ := f.GetCellType(sheet, ref); t == excelize.CellTypeUnset || t == excelize.CellTypeNumber {
				r.values[col] = strconv.FormatFloat(n, 'f', -1, 64)
			}
		}
		if p, ok := validScheduleRow(r, problem); ok {
			valid = append(valid, importedRow{row: r.row, class: scheduleClassKey{r.values[colGrade], r.values[colSection]}, period: p})
		}
	}
	return valid, headerFound, ""
}

// matchScheduleHeader reports whether cells is the header row: at least three of the six header
// names. It returns the sheet column of each field, or nil after reporting a missing or repeated
// header name.
func matchScheduleHeader(cells []string, rowNum int, problem func(row, column int, msg string)) ([]int, bool) {
	cols := make([]int, len(scheduleColumns))
	for i := range cols {
		cols[i] = -1
	}
	matches := 0
	repeated := map[int]bool{}
	for c, v := range cells {
		k, ok := scheduleHeaderAliases[normalizeArabic(v)]
		if !ok {
			continue
		}
		if cols[k] >= 0 {
			repeated[k] = true
			continue
		}
		cols[k] = c
		matches++
	}
	if matches < scheduleHeaderMinMatches {
		return nil, false
	}
	complete := len(repeated) == 0
	for k, c := range cols {
		switch {
		case repeated[k]:
			problem(rowNum, k, fmt.Sprintf("the header row has the column %s more than once", scheduleColumns[k].name))
		case c < 0:
			problem(rowNum, k, fmt.Sprintf("the header row has no column %s", scheduleColumns[k].name))
			complete = false
		}
	}
	if !complete {
		return nil, true
	}
	return cols, true
}

var easternDigits = strings.NewReplacer(
	"٠", "0", "١", "1", "٢", "2", "٣", "3", "٤", "4", "٥", "5", "٦", "6", "٧", "7", "٨", "8", "٩", "9",
	"۰", "0", "۱", "1", "۲", "2", "۳", "3", "۴", "4", "۵", "5", "۶", "6", "۷", "7", "۸", "8", "۹", "9",
)

func validScheduleRow(r rawScheduleRow, problem func(row, column int, msg string)) (SchedulePeriod, bool) {
	ok := true
	bad := func(column int, msg string) {
		problem(r.row, column, msg)
		ok = false
	}
	grade, section := r.values[colGrade], r.values[colSection]
	for _, c := range []struct {
		column      int
		name, value string
	}{{colGrade, "grade", grade}, {colSection, "section", section}} {
		if c.value == "" {
			bad(c.column, c.name+" is required")
		} else if msg := tooLong(c.name, c.value, 50); msg != "" {
			bad(c.column, msg)
		}
	}
	day, dayOK := canonicalSchoolDay(r.values[colDay])
	if !dayOK {
		bad(colDay, "day must be a school day, Sunday to Thursday (Arabic or English)")
	}
	period, err := strconv.ParseFloat(easternDigits.Replace(r.values[colPeriod]), 64)
	if err != nil || period != math.Trunc(period) || period < 1 || period > MaxPeriodNumber {
		bad(colPeriod, fmt.Sprintf("period must be a whole number from 1 to %d", MaxPeriodNumber))
	}
	subject, teacher := r.values[colSubject], r.values[colTeacher]
	if subject == "" {
		bad(colSubject, "subject is required when a teacher is given")
	} else if msg := tooLong("subject", subject, 100); msg != "" {
		bad(colSubject, msg)
	}
	if msg := tooLong("teacher", teacher, 100); msg != "" {
		bad(colTeacher, msg)
	}
	p := SchedulePeriod{DayOfWeek: day, PeriodNumber: int(period), SubjectName: subject}
	if teacher != "" {
		p.TeacherName = &teacher
	}
	return p, ok
}

var scheduleImportPartLimits = map[string]int64{"file": MaxScheduleFileBytes, "dry_run": 16, "grade": maxScheduleClassPartBytes, "section": maxScheduleClassPartBytes}

func scheduleImportOverLimit(name string) *formError {
	switch name {
	case "file":
		return &formError{status: http.StatusRequestEntityTooLarge, msg: fmt.Sprintf("file must be at most 2 MiB (%d bytes)", MaxScheduleFileBytes),
			warn: "Request body too large", warnLimit: MaxScheduleFileBytes}
	case "grade", "section":
		return &formError{status: http.StatusBadRequest, msg: name + " must be at most 50 characters"}
	}
	return &formError{status: http.StatusBadRequest, msg: `dry_run must be "true" or "false"`}
}

// importClassGuard returns the class the grade and section parts name, nil when neither is sent,
// or a problem.
func importClassGuard(raw map[string][]byte) (*scheduleClassKey, string) {
	grade, hasGrade := raw["grade"]
	section, hasSection := raw["section"]
	switch {
	case !hasGrade && !hasSection:
		return nil, ""
	case !hasSection:
		return nil, "section is required when grade is sent"
	case !hasGrade:
		return nil, "grade is required when section is sent"
	}
	for _, part := range []struct {
		name  string
		value []byte
	}{{"grade", grade}, {"section", section}} {
		if !utf8.Valid(part.value) || bytes.ContainsRune(part.value, 0) {
			return nil, part.name + " must be text"
		}
	}
	k := scheduleClassKey{strings.TrimSpace(string(grade)), strings.TrimSpace(string(section))}
	if msg := scheduleClassProblem(k.grade, k.section); msg != "" {
		return nil, msg
	}
	return &k, ""
}

// capImportErrors keeps the first MaxScheduleImportErrors problems and adds one saying how many
// more there are.
func capImportErrors(problems []ScheduleImportError) []ScheduleImportError {
	if len(problems) <= MaxScheduleImportErrors {
		return problems
	}
	more := len(problems) - MaxScheduleImportErrors
	return append(problems[:MaxScheduleImportErrors:MaxScheduleImportErrors], ScheduleImportError{Message: fmt.Sprintf("and %d more problems", more)})
}

func (app *AppEnv) AdminScheduleImportHandler(w http.ResponseWriter, r *http.Request) {
	extendUploadDeadlines(w)
	raw, ferr := readFormParts(w, r, MaxScheduleImportBodyBytes, scheduleImportPartLimits, scheduleImportOverLimit)
	if ferr != nil {
		ferr.respond(w, r)
		return
	}
	file, ok := raw["file"]
	if !ok {
		respondError(w, http.StatusBadRequest, "file is required")
		return
	}
	dryRun := false
	if v, ok := raw["dry_run"]; ok {
		switch string(v) {
		case "true":
			dryRun = true
		case "false":
		default:
			respondError(w, http.StatusBadRequest, `dry_run must be "true" or "false"`)
			return
		}
	}

	only, msg := importClassGuard(raw)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}

	imp, msg := readScheduleWorkbook(file, only)
	if msg != "" {
		respondError(w, http.StatusBadRequest, msg)
		return
	}
	if n := len(imp.problems); n > 0 {
		noun := "problems"
		if n == 1 {
			noun = "problem"
		}
		respondJSON(w, http.StatusBadRequest, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("The file has %d %s; nothing was saved", n, noun),
			"errors":  capImportErrors(imp.problems),
		})
		return
	}

	keys := make([]scheduleClassKey, 0, len(imp.classes))
	for k := range imp.classes {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].grade != keys[j].grade {
			return keys[i].grade < keys[j].grade
		}
		return keys[i].section < keys[j].section
	})
	classes := make([]ScheduleImportClass, len(keys))
	total := 0
	err := func() error {
		tx, err := app.DB.BeginTx(r.Context(), nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := lockSchedules(r.Context(), tx); err != nil {
			return err
		}
		for i, k := range keys {
			periods := imp.classes[k]
			if err := replaceClassSchedule(r.Context(), tx, k.grade, k.section, periods); err != nil {
				return err
			}
			classes[i] = ScheduleImportClass{Grade: k.grade, Section: k.section, Periods: len(periods)}
			if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM students WHERE is_active = true AND grade = $1 AND section = $2`, k.grade, k.section).Scan(&classes[i].MatchedStudents); err != nil {
				return err
			}
			total += len(periods)
		}
		if dryRun {
			return nil
		}
		return tx.Commit()
	}()
	if err != nil {
		respondInternalError(w, "Failed to import schedule", "AdminScheduleImportHandler: import failed", err, "dry_run", dryRun, "classes", len(keys))
		return
	}

	warnings := []string{}
	for _, c := range classes {
		if c.MatchedStudents == 0 {
			warnings = append(warnings, fmt.Sprintf("grade %s, section %s: no active student has this grade and section, parents will not see it", c.Grade, c.Section))
		}
	}
	message := "Schedule imported"
	if dryRun {
		message = "Dry run: the file is valid; nothing was saved"
	}
	slog.Info("Schedule imported", "dry_run", dryRun, "classes", len(classes), "total_periods", total)
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "success",
		"message":       message,
		"dry_run":       dryRun,
		"total_periods": total,
		"classes":       classes,
		"warnings":      warnings,
	})
}
