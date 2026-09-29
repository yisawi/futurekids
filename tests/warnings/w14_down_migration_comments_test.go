package warnings

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// w14Rollback is the hand-audited impact of one down-migration. usage matches SQL in Go code
// that depends on what the rollback removes: for a warning, every matching Go file must be
// named in its header; for "none", nothing may match.
type w14Rollback struct {
	impact  bool
	removes []string
	usage   string
}

var w14Rollbacks = map[string]w14Rollback{
	"000001": {true, []string{"attendance_logs", "devices", "students"}, `(?:FROM|JOIN|INTO|UPDATE)\s+(?:attendance_logs|devices|students)\b`},
	"000002": {true, []string{"weekly_schedules", "students.grade", "students.section"}, `(?:FROM|JOIN|INTO|UPDATE)\s+weekly_schedules\b|\bs\.(?:grade|section)\b|, grade, section|COALESCE\((?:s\.)?(?:grade|section)\b`},
	"000003": {true, []string{"notifications"}, `(?:FROM|JOIN|INTO|UPDATE)\s+notifications\b`},
	"000004": {true, []string{"students.avatar_url"}, `\bavatar_url\b`},
	"000005": {true, []string{"student_leaves", "attendance_logs.status"}, `(?:FROM|JOIN|INTO|UPDATE)\s+student_leaves\b`},
	"000006": {false, nil, `\bs\.parent_pin\b|students\.parent_pin|students \([^)]*parent_pin`},
	"000007": {true, []string{"banners"}, `(?:FROM|JOIN|INTO|UPDATE)\s+banners\b`},
	"000008": {true, []string{"admins"}, `(?:FROM|JOIN|INTO|UPDATE)\s+admins\b`},
	"000009": {true, []string{"parents", "students.parent_id"}, `(?:FROM|JOIN|INTO|UPDATE)\s+parents\b|\bs\.parent_id\b|WHERE parent_id = \$`},
	"000010": {true, []string{"settings"}, `(?:FROM|INTO)\s+settings\b`},
	"000011": {false, nil, `\bs\.parent_phone\b|students\.parent_phone|students \([^)]*parent_phone`},
	"000012": {false, nil, ""},
	"000013": {true, []string{"students.is_active"}, `\bs\.is_active\b|parent_id = \$1 AND is_active|students SET is_active`},
	"000014": {true, []string{"get_student_status"}, `get_student_status`},
	"000015": {true, []string{"get_student_status", "first_check TEXT", "last_check TEXT"}, `\b(?:first_check|last_check)\b`},
	"000016": {false, nil, ""},
	"000017": {false, nil, ""},
	"000018": {false, nil, ""},
	"000019": {false, nil, `check_time, status\)|\bstatus\) VALUES|a\.status\b`},
	"000020": {false, nil, ""},
}

var (
	w14PathRE    = regexp.MustCompile(`\b((?:internal|cmd|db)/[A-Za-z0-9_./-]+\.(?:go|sql|md))\b`)
	w14DropRE    = regexp.MustCompile(`(?i)\bDROP\s+(?:TABLE|COLUMN|FUNCTION)\b`)
	w14ReturnsRE = regexp.MustCompile(`(?s)RETURNS TABLE \((.*?)\)`)
)

// w14Header returns the leading comment block of a SQL file.
func w14Header(sqlText string) string {
	var lines []string
	for _, l := range strings.Split(sqlText, "\n") {
		if !strings.HasPrefix(l, "--") {
			break
		}
		lines = append(lines, l)
	}
	return strings.Join(lines, "\n")
}

// w14GoUsers lists non-test Go files (repo-relative) whose content matches pattern.
func w14GoUsers(t *testing.T, root, pattern string) []string {
	t.Helper()
	if pattern == "" {
		return nil
	}
	re := regexp.MustCompile(pattern)
	var users []string
	for _, glob := range []string{"internal/*/*.go", "cmd/*/*.go"} {
		files, _ := filepath.Glob(filepath.Join(root, glob))
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatalf("read %s: %v", f, err)
			}
			if re.Match(src) {
				rel, _ := filepath.Rel(root, f)
				users = append(users, filepath.ToSlash(rel))
			}
		}
	}
	sort.Strings(users)
	return users
}

// w14Problems checks one down-migration header against its audited impact.
func w14Problems(root, header string, rb w14Rollback, users []string) []string {
	var problems []string
	if rb.impact {
		lines := strings.Split(header, "\n")
		if len(lines) == 0 || lines[0] != "-- ⚠ APP-COMPATIBILITY WARNING" {
			problems = append(problems, "first line must be '-- ⚠ APP-COMPATIBILITY WARNING'")
		}
		for _, want := range []string{"-- Removes:", "will break:", "-- Data loss:", "back up the database", "pg_dump", "redeploy"} {
			if !strings.Contains(header, want) {
				problems = append(problems, "header lacks "+strings.TrimPrefix(want, "-- "))
			}
		}
		for _, obj := range rb.removes {
			if !strings.Contains(header, obj) {
				problems = append(problems, "header does not name removed object "+obj)
			}
		}
		for _, u := range users {
			if !strings.Contains(header, u) {
				problems = append(problems, u+" depends on what this rollback removes but is not named in the warning")
			}
		}
		named := w14PathRE.FindAllStringSubmatch(header, -1)
		if len(named) == 0 {
			problems = append(problems, "warning names no dependent file")
		}
		for _, m := range named {
			if _, err := os.Stat(filepath.Join(root, m[1])); err != nil {
				problems = append(problems, "warning names "+m[1]+", which does not exist")
			}
		}
		return problems
	}

	const prefix = "-- APP-COMPATIBILITY: none — "
	if !strings.HasPrefix(header, prefix) || len(strings.SplitN(header, "\n", 2)[0]) < len(prefix)+20 {
		problems = append(problems, "no-impact rollback must start with '"+prefix+"<reason>'")
	}
	if !strings.Contains(header, "-- Data loss:") {
		problems = append(problems, "header lacks Data loss:")
	}
	for _, u := range users {
		problems = append(problems, "claims no app impact, but "+u+" uses what it removes")
	}
	return problems
}

// TestW14DownMigrationWarnings verifies audit warning W14 (RULES.md §3): every down-migration
// starts with an app-compatibility header; each rollback that removes something the current Go
// code uses carries a warning naming every such file, the data lost, and back-up/redeploy
// instructions; the rest state explicitly why they are harmless. Up-migrations are not checked.
func TestW14DownMigrationWarnings(t *testing.T) {
	root := filepath.Join("..", "..")
	downs, _ := filepath.Glob(filepath.Join(root, "db", "migrations", "*.down.sql"))
	ups, _ := filepath.Glob(filepath.Join(root, "db", "migrations", "*.up.sql"))
	if len(downs) == 0 || len(downs) != len(ups) {
		t.Fatalf("found %d down and %d up migrations; every up needs a matching down", len(downs), len(ups))
	}

	failing := 0
	seen := map[string]bool{}
	for _, f := range downs {
		name := filepath.Base(f)
		num := name[:6]
		seen[num] = true
		t.Run(name, func(t *testing.T) {
			rb, ok := w14Rollbacks[num]
			if !ok {
				failing++
				t.Fatalf("new down-migration: audit its app impact and add it to w14Rollbacks")
			}
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			header := w14Header(string(src))
			body := strings.TrimPrefix(string(src), header)
			problems := w14Problems(root, header, rb, w14GoUsers(t, root, rb.usage))

			if !rb.impact && w14DropRE.MatchString(body) && rb.usage == "" && num != "000017" {
				problems = append(problems, "drops a table, column or function but is marked no-impact without a usage check")
			}
			if num == "000017" {
				up, _ := os.ReadFile(filepath.Join(root, "db", "migrations", "000017_close_time_window_gaps.up.sql"))
				a, b := w14ReturnsRE.FindStringSubmatch(string(up)), w14ReturnsRE.FindStringSubmatch(body)
				if a == nil || b == nil || strings.Join(strings.Fields(a[1]), " ") != strings.Join(strings.Fields(b[1]), " ") {
					problems = append(problems, "claims no impact, but the recreated get_student_status signature differs from 000017 up")
				}
			}
			for _, p := range problems {
				t.Error(p)
			}
			if len(problems) > 0 {
				failing++
			}
		})
	}
	for num := range w14Rollbacks {
		if !seen[num] {
			t.Errorf("w14Rollbacks lists %s, which has no down-migration file", num)
		}
	}

	t.Run("checker catches a warning that misses a dependent file", func(t *testing.T) {
		rb := w14Rollbacks["000007"]
		src, _ := os.ReadFile(filepath.Join(root, "db", "migrations", "000007_add_banners_table.down.sql"))
		header := strings.ReplaceAll(w14Header(string(src)), "internal/handlers/mobile.go", "(file removed)")
		if p := w14Problems(root, header, rb, w14GoUsers(t, root, rb.usage)); len(p) == 0 {
			t.Error("a warning that no longer names internal/handlers/mobile.go passed the check")
		}
		if p := w14Problems(root, "-- APP-COMPATIBILITY: none — nothing reads the banners table, surely.\n-- Data loss: none.", rb, w14GoUsers(t, root, rb.usage)); len(p) == 0 {
			t.Error("a false 'none' claim for banners passed the check")
		}
	})

	t.Run("rollback guide exists", func(t *testing.T) {
		readme, err := os.ReadFile(filepath.Join(root, "db", "migrations", "README.md"))
		if err != nil || !strings.Contains(string(readme), "## Rolling back") || !strings.Contains(string(readme), "pg_dump") {
			t.Errorf("db/migrations/README.md must have a 'Rolling back' section with backup instructions (err=%v)", err)
		}
	})

	t.Logf("checked %d down-migrations (%d failing); %d up-migrations need no header", len(downs), failing, len(ups))
	if t.Failed() {
		t.Log("FAIL: Down-migration app-compatibility headers are missing or wrong (see subtest errors above)")
	} else {
		t.Log("PASS: Every down-migration states its app impact; warnings name every dependent file")
	}
}
