package main

import (
	"database/sql"
	"fmt"
	"log"

	_ "github.com/jackc/pgx/v5/stdlib"
)

func main() {
	db, err := sql.Open("pgx", "postgres://yisawi@localhost:5432/future_kids?sslmode=disable")
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	query := `
		SELECT 
			s.id, 
			s.full_name, 
			CASE st.status
				WHEN 'Present' THEN 'حاضر'
				WHEN 'Excused' THEN 'مجاز'
				ELSE 'غائب'
			END as status
		FROM students s
		CROSS JOIN LATERAL get_student_status(s.id, '2023-10-10'::DATE) st
	`
	_, err = db.Query(query)
	if err != nil {
		fmt.Println("DB_ERROR:", err)
	} else {
		fmt.Println("SUCCESS!")
	}
}
