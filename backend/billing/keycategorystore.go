package billing

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// KeyCategory 资源类别。台子通过 key_stages.category_ids（JSONB 数组）多选绑定类别。
type KeyCategory struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
	// StageCount 绑定了该类别的台子数，删除前用来提示影响范围。
	StageCount int `json:"stageCount"`
}

// cleanCategoryIDs 去掉非法 ID 并去重保序；始终返回非 nil，保证 JSON 是 [] 而不是 null。
func cleanCategoryIDs(in []int64) []int64 {
	seen := map[int64]bool{}
	out := make([]int64, 0, len(in))
	for _, id := range in {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

// ListKeyCategories 类别列表，按序号升序，附各自绑定的台子数。
func ListKeyCategories(cfg PGConfig) ([]KeyCategory, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT c.id, c.name, c.description, c.created_at, c.updated_at,
		       (SELECT COUNT(*) FROM key_stages s WHERE s.category_ids @> jsonb_build_array(c.id)) AS stage_count
		FROM key_categories c ORDER BY c.id
	`)
	if err != nil {
		return nil, fmt.Errorf("查询资源类别失败: %w", err)
	}
	defer rows.Close()

	out := []KeyCategory{}
	for rows.Next() {
		var c KeyCategory
		if err := rows.Scan(&c.ID, &c.Name, &c.Description, &c.CreatedAt, &c.UpdatedAt, &c.StageCount); err != nil {
			return nil, fmt.Errorf("读取资源类别失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取资源类别失败: %w", err)
	}
	return out, nil
}

// UpsertKeyCategory 新增或更新类别。ID 为 0 表示新增；名称不可重复。
func UpsertKeyCategory(cfg PGConfig, c KeyCategory) (KeyCategory, error) {
	c.Name = strings.TrimSpace(c.Name)
	c.Description = strings.TrimSpace(c.Description)
	if c.Name == "" {
		return KeyCategory{}, fmt.Errorf("类别名称不能为空")
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return KeyCategory{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if c.ID == 0 {
		err = db.QueryRow(`
			INSERT INTO key_categories (name, description) VALUES ($1, $2)
			RETURNING id, created_at, updated_at
		`, c.Name, c.Description).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
	} else {
		err = db.QueryRow(`
			UPDATE key_categories SET name = $2, description = $3, updated_at = now()
			WHERE id = $1
			RETURNING created_at, updated_at
		`, c.ID, c.Name, c.Description).Scan(&c.CreatedAt, &c.UpdatedAt)
		if err == sql.ErrNoRows {
			return KeyCategory{}, fmt.Errorf("资源类别不存在（可能已被删除）")
		}
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return KeyCategory{}, fmt.Errorf("类别名称「%s」已存在", c.Name)
		}
		return KeyCategory{}, fmt.Errorf("保存资源类别失败: %w", err)
	}
	return c, nil
}

// DeleteKeyCategory 删除类别，并在同一事务里把它从所有台子的绑定中摘掉，
// 避免台子上留着指向已删类别的悬空 ID。
func DeleteKeyCategory(cfg PGConfig, id int64) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("删除资源类别失败: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM key_categories WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除资源类别失败: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("删除资源类别失败: %w", err)
	} else if n == 0 {
		return fmt.Errorf("资源类别不存在（可能已被删除）")
	}

	if _, err := tx.Exec(`
		UPDATE key_stages
		SET category_ids = COALESCE((
			SELECT jsonb_agg(e) FROM jsonb_array_elements(category_ids) e
			WHERE e <> to_jsonb($1::bigint)
		), '[]'::jsonb)
		WHERE category_ids @> jsonb_build_array($1::bigint)
	`, id); err != nil {
		return fmt.Errorf("解除台子上的类别绑定失败: %w", err)
	}
	return tx.Commit()
}
