package warnings

import (
	"bytes"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"future_kids/internal/testdb"
)

// bkTools are the programs the backup script and this test need; the test skips without them.
var bkTools = []string{"bash", "pg_dump", "pg_restore", "age", "age-keygen"}

// bkRun runs scripts/db-backup.sh with exactly env (plus PATH and TMPDIR) and returns its exit
// code and combined output.
func bkRun(t *testing.T, path, tmp string, env map[string]string) (int, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join("..", "..", "scripts", "db-backup.sh"))
	cmd.Env = []string{"PATH=" + path, "TMPDIR=" + tmp, "HOME=" + tmp}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run script: %v", err)
	}
	return code, string(out)
}

// bkFiles lists the regular files under dir, relative.
func bkFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel(dir, p)
			files = append(files, rel)
		}
		return nil
	})
	return files
}

// bkPlaintextDumps returns the files under dirs that start like a pg_dump custom-format archive.
func bkPlaintextDumps(t *testing.T, dirs ...string) []string {
	t.Helper()
	var found []string
	for _, dir := range dirs {
		for _, f := range bkFiles(t, dir) {
			b, err := os.ReadFile(filepath.Join(dir, f))
			if err == nil && bytes.HasPrefix(b, []byte("PGDMP")) {
				found = append(found, f)
			}
		}
	}
	return found
}

// bkCounts returns "table=rows" for every table in the public schema of db, sorted by name.
func bkCounts(t *testing.T, db *sql.DB) []string {
	t.Helper()
	rows, err := db.Query(`SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		rows.Scan(&name)
		tables = append(tables, name)
	}
	rows.Close()
	var counts []string
	for _, tb := range tables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM "` + tb + `"`).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tb, err)
		}
		counts = append(counts, fmt.Sprintf("%s=%d", tb, n))
	}
	return counts
}

// bkEmptyDB creates an empty throwaway database (fk_test_ prefix, local server only, as
// internal/testdb requires) and returns a pool and its URL; it is dropped when t ends.
func bkEmptyDB(t *testing.T, label string) (*sql.DB, string) {
	t.Helper()
	admin, err := testdb.ValidateAdminURL(os.Getenv(testdb.EnvVar))
	if err != nil {
		t.Fatalf("testdb: %v", err)
	}
	adminDB := openDB(t, admin.String())
	name := testdb.Name(label, time.Now())
	if _, err := adminDB.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { adminDB.Exec("DROP DATABASE IF EXISTS " + name + " WITH (FORCE)") })
	u := *admin
	u.Path = "/" + name
	return openDB(t, u.String()), u.String()
}

// TestBackupScript verifies scripts/db-backup.sh: it refuses missing or invalid input and missing
// tools before touching the database, writes only an age-encrypted dump that restores to the same
// row counts, never leaves a plaintext dump on disk, and never prints the database URL or password.
func TestBackupScript(t *testing.T) {
	for _, tool := range bkTools {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed; install age and the PostgreSQL client to run this test", tool)
		}
	}
	path := os.Getenv("PATH")
	tmp, outDir, keyDir := t.TempDir(), t.TempDir(), t.TempDir()

	src, srcDSN := setupThrowawayDB(t, "bkpsrc")
	k1Exec(t, src,
		`INSERT INTO parents (id, full_name, phone_number, pin_code) VALUES (8501, 'BK Parent', '+9647000008501', 'x'), (8502, 'BK Parent 2', '+9647000008502', 'x')`,
		`INSERT INTO students (id, full_name, rfid_tag, parent_id, grade, section) SELECT 8500 + g, 'BK Kid ' || g, (85000 + g)::text, 8501 + g % 2, 'G1', 'A' FROM generate_series(1, 40) g`,
		`INSERT INTO devices (serial_number, location_name, is_active) VALUES ('BK-DEV', 'Gate', true)`,
		`INSERT INTO attendance_logs (student_id, device_sn, check_time) SELECT 8500 + g, 'BK-DEV', TIMESTAMP '2026-09-01 07:15' + g * INTERVAL '1 day' FROM generate_series(1, 40) g`,
		`INSERT INTO notifications (parent_id, title, body) SELECT 8501, 'BK', 'note ' || g FROM generate_series(1, 25) g`)

	keyFile := filepath.Join(keyDir, "key.txt")
	if out, err := exec.Command("age-keygen", "-o", keyFile).CombinedOutput(); err != nil {
		t.Fatalf("age-keygen: %v %s", err, out)
	}
	pubOut, err := exec.Command("age-keygen", "-y", keyFile).Output()
	if err != nil {
		t.Fatal(err)
	}
	recipient := strings.TrimSpace(string(pubOut))

	// A URL with a password: the local server's trust authentication accepts any password.
	const password = "bk-Secret-Pw-91x"
	u, _ := url.Parse(srcDSN)
	osUser, _ := user.Current()
	u.User = url.UserPassword(osUser.Username, password)
	q := u.Query()
	q.Set("sslmode", "disable")
	u.RawQuery = q.Encode()
	dbURL := u.String()
	good := map[string]string{"BACKUP_DATABASE_URL": dbURL, "BACKUP_AGE_RECIPIENT": recipient, "BACKUP_OUT_DIR": outDir, "BACKUP_LABEL": "staging"}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range good {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(m, kv[i])
			} else {
				m[kv[i]] = kv[i+1]
			}
		}
		return m
	}
	noSecrets := func(t *testing.T, out string, extra ...string) {
		t.Helper()
		for _, s := range append([]string{dbURL, password, u.Host + u.Path, recipient}, extra...) {
			if s != "" && strings.Contains(out, s) {
				t.Errorf("output contains a secret or connection detail (%d characters):\n%s", len(s), out)
			}
		}
	}

	t.Run("refusals happen before any dump and leave no file", func(t *testing.T) {
		// A PATH of links to the tools the script needs, without pg_dump or without age.
		linked := func(skip string) string {
			dir := t.TempDir()
			for _, tool := range []string{"bash", "env", "date", "mkdir", "mktemp", "rm", "mv", "wc", "tr", "cat", "pg_dump", "age"} {
				if tool == skip {
					continue
				}
				p, err := exec.LookPath(tool)
				if err != nil {
					t.Fatalf("%s: %v", tool, err)
				}
				os.Symlink(p, filepath.Join(dir, tool))
			}
			return dir
		}
		for _, c := range []struct {
			name, path string
			env        map[string]string
			msg        string
		}{
			{"no URL", path, with("BACKUP_DATABASE_URL", ""), "missing environment variable(s): BACKUP_DATABASE_URL"},
			{"no recipient", path, with("BACKUP_AGE_RECIPIENT", ""), "missing environment variable(s): BACKUP_AGE_RECIPIENT"},
			{"no output directory", path, with("BACKUP_OUT_DIR", ""), "missing environment variable(s): BACKUP_OUT_DIR"},
			{"no label", path, with("BACKUP_LABEL", ""), "missing environment variable(s): BACKUP_LABEL"},
			{"nothing set", path, map[string]string{}, "missing environment variable(s): BACKUP_DATABASE_URL BACKUP_AGE_RECIPIENT BACKUP_OUT_DIR BACKUP_LABEL"},
			{"label prod", path, with("BACKUP_LABEL", "prod"), "BACKUP_LABEL must be staging or production"},
			{"short recipient", path, with("BACKUP_AGE_RECIPIENT", "age1short"), "BACKUP_AGE_RECIPIENT is not an age public key"},
			{"ssh key as recipient", path, with("BACKUP_AGE_RECIPIENT", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"), "BACKUP_AGE_RECIPIENT is not an age public key"},
			{"uppercase recipient", path, with("BACKUP_AGE_RECIPIENT", strings.ToUpper(recipient)), "BACKUP_AGE_RECIPIENT is not an age public key"},
			{"the age identity instead of the recipient", path, with("BACKUP_AGE_RECIPIENT", "AGE-SECRET-KEY-1QQQ"), "BACKUP_AGE_RECIPIENT is not an age public key"},
			{"pg_dump missing", linked("pg_dump"), good, "pg_dump is not installed"},
			{"age missing", linked("age"), good, "age is not installed"},
			{"not a postgres URL", path, with("BACKUP_DATABASE_URL", "mysql://u:"+password+"@db.example.com/x"), "BACKUP_DATABASE_URL must be a postgres:// or postgresql:// URL"},
		} {
			code, out := bkRun(t, c.path, tmp, c.env)
			if code != 2 || !strings.Contains(out, c.msg) {
				t.Errorf("%s: exit %d, output %q; want exit 2 and %q", c.name, code, out, c.msg)
			}
			noSecrets(t, out, "mysql://")
			if files := bkFiles(t, outDir); len(files) != 0 {
				t.Errorf("%s: files left: %v", c.name, files)
			}
		}
	})

	t.Run("a failing dump prints redacted errors and leaves no file", func(t *testing.T) {
		bad := "postgresql://bk-user-xyz:" + password + "@localhost:1/bk_missing_db?sslmode=disable"
		code, out := bkRun(t, path, tmp, with("BACKUP_DATABASE_URL", bad))
		if code != 1 || !strings.Contains(out, "pg_dump failed") || !strings.Contains(out, "<redacted>") {
			t.Errorf("exit %d, output:\n%s", code, out)
		}
		noSecrets(t, out, bad, "bk-user-xyz", "bk_missing_db")
		if files := bkFiles(t, outDir); len(files) != 0 {
			t.Errorf("files left: %v", files)
		}
		if leftovers := bkFiles(t, tmp); len(leftovers) != 0 {
			t.Errorf("temporary files left: %v", leftovers)
		}
	})

	t.Run("backup, decrypt and restore give the same row counts", func(t *testing.T) {
		code, out := bkRun(t, path, tmp, good)
		if code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		noSecrets(t, out)
		files := bkFiles(t, outDir)
		name := regexp.MustCompile(`^futurekids-staging-\d{8}T\d{6}Z\.dump\.age$`)
		if len(files) != 1 || !name.MatchString(files[0]) || !strings.Contains(out, "Backup written: "+files[0]) {
			t.Fatalf("output files %v, output %q", files, out)
		}
		file := filepath.Join(outDir, files[0])
		info, _ := os.Stat(file)
		raw, _ := os.ReadFile(file)
		if info.Mode().Perm() != 0o600 || info.Size() < 1024 || !bytes.HasPrefix(raw, []byte("age-encryption.org/v1")) {
			t.Errorf("file mode %v, size %d, header %q", info.Mode().Perm(), info.Size(), raw[:min(len(raw), 22)])
		}
		if dumps := bkPlaintextDumps(t, outDir, tmp); len(dumps) != 0 {
			t.Errorf("plaintext dump files on disk: %v", dumps)
		}
		if leftovers := bkFiles(t, tmp); len(leftovers) != 0 {
			t.Errorf("temporary files left: %v", leftovers)
		}

		restored, restoreURL := bkEmptyDB(t, "bkprestore")
		pipe := exec.Command("bash", "-c", `set -o pipefail; age -d -i "$KEY" "$FILE" | pg_restore --no-owner --no-acl -d "$TARGET"`)
		pipe.Env = append(os.Environ(), "KEY="+keyFile, "FILE="+file, "TARGET="+restoreURL)
		if out, err := pipe.CombinedOutput(); err != nil {
			t.Fatalf("decrypt and restore: %v\n%s", err, out)
		}
		want, got := bkCounts(t, src), bkCounts(t, restored)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("row counts differ:\nsource   %v\nrestored %v", want, got)
		}
		if !strings.Contains(strings.Join(got, ","), "students=40") || !strings.Contains(strings.Join(got, ","), "attendance_logs=40") {
			t.Errorf("restored counts %v miss the seeded rows", got)
		}
		t.Logf("restored %d tables with identical row counts", len(got))

		if out, err := exec.Command("age", "-d", "-i", filepath.Join(keyDir, "nope"), file).CombinedOutput(); err == nil {
			t.Errorf("decrypting without the key succeeded: %s", out)
		}
	})
}
