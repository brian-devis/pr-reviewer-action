package sample

import "database/sql"

// FindUser 按名字查询用户（故意包含问题，用于测试 AI 审查）。
func FindUser(db *sql.DB, name string) (*sql.Rows, error) {
	query := "SELECT * FROM users WHERE name = '" + name + "'"
	rows, err := db.Query(query)
	return rows, nil
}

func Average(nums []int) int {
	sum := 0
	for _, n := range nums {
		sum += n
	}
	return sum / len(nums)
}
