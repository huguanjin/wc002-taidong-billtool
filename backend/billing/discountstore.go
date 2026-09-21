package billing

import (
	"database/sql"
	"fmt"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// PGConfig 本地 PostgreSQL 连接信息，用于持久化「拉取客户折扣」的结果快照。
// 与业务库 DBConfig 是两个完全独立的数据库：这里只读写 billtool 自己的数据，
// 不会碰到业务库的任何表。
type PGConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	DBName   string
	SSLMode  string // 默认 "disable"
}

func (c PGConfig) dsn() string {
	port := c.Port
	if port == "" {
		port = "5432"
	}
	sslMode := c.SSLMode
	if sslMode == "" {
		sslMode = "disable"
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%s/%s?sslmode=%s",
		c.User, c.Password, c.Host, port, c.DBName, sslMode)
}

// EnsureUserDiscountSchema 建表（幂等），启动时调用一次。
func EnsureUserDiscountSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS user_discount_pulls (
			id BIGSERIAL PRIMARY KEY,
			user_id INTEGER NOT NULL,
			username TEXT NOT NULL,
			user_group TEXT NOT NULL,
			group_ratio DOUBLE PRECISION NOT NULL,
			discount DOUBLE PRECISION NOT NULL,
			discount_base_factor DOUBLE PRECISION NOT NULL,
			derived_from TEXT NOT NULL,
			fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 user_discount_pulls 表失败: %w", err)
	}
	if _, err := db.Exec(`
		CREATE INDEX IF NOT EXISTS idx_user_discount_pulls_user_id
		ON user_discount_pulls (user_id, fetched_at DESC)
	`); err != nil {
		return fmt.Errorf("创建 user_discount_pulls 索引失败: %w", err)
	}
	return nil
}

// SaveUserDiscountPull 追加写入一次拉取结果（不覆盖，保留历史快照）。
func SaveUserDiscountPull(cfg PGConfig, r UserDiscountResult) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		INSERT INTO user_discount_pulls
			(user_id, username, user_group, group_ratio, discount, discount_base_factor, derived_from, fetched_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, r.UserID, r.Username, r.UserGroup, r.GroupRatio, r.Discount, DiscountBaseFactor, r.DerivedFrom, r.FetchedAt)
	if err != nil {
		return fmt.Errorf("写入折扣快照失败: %w", err)
	}
	return nil
}

// RecentUserDiscountPulls 按 user_id 查最近 limit 条拉取记录，按时间倒序。
func RecentUserDiscountPulls(cfg PGConfig, userID int, limit int) ([]UserDiscountResult, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT user_id, username, user_group, group_ratio, discount, derived_from, fetched_at
		FROM user_discount_pulls
		WHERE user_id = $1
		ORDER BY fetched_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("查询折扣历史失败: %w", err)
	}
	defer rows.Close()

	var result []UserDiscountResult
	for rows.Next() {
		var r UserDiscountResult
		var fetchedAt time.Time
		if err := rows.Scan(&r.UserID, &r.Username, &r.UserGroup, &r.GroupRatio, &r.Discount, &r.DerivedFrom, &fetchedAt); err != nil {
			return nil, fmt.Errorf("读取折扣历史失败: %w", err)
		}
		r.FetchedAt = fetchedAt
		result = append(result, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取折扣历史失败: %w", err)
	}
	return result, nil
}
