package tests

import (
	"context"
	"database/sql"
	"fmt"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func runQuery() {
	db, err := sql.Open("pgx", "postgres://yisawi@localhost:5432/future_kids?sslmode=disable")
	if err != nil {
		panic(err)
	}
	defer db.Close()
	query := `
		SELECT 
			s.id, 
			s.full_name, 
			COALESCE(s.grade, 'غير محدد'), 
			COALESCE(s.section, '-'), 
			p.full_name as parent_name, 
			p.phone_number,
			CASE st.status
				WHEN 'Present' THEN 'حاضر'
				WHEN 'Excused' THEN 'مجاز'
				ELSE 'غائب'
			END as status,
			COALESCE(TO_CHAR(st.first_check, 'HH24:MI'), '') as check_time
		FROM students s
		JOIN parents p ON s.parent_id = p.id
		CROSS JOIN LATERAL get_student_status(s.id, $1::DATE) st
		WHERE s.is_active = true
		ORDER BY st.status DESC, s.full_name ASC
	`
	_, err = db.QueryContext(context.Background(), query, "2026-09-14")
	if err != nil {
		fmt.Println("DB ERROR IN TEST:", err)
	} else {
		fmt.Println("SUCCESS")
	}
}
