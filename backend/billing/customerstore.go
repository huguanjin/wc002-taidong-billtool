package billing

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 客户基本信息：把「客户名称」和它的业务库账号列表绑在一起。
//
// 存在的理由：出账时要填的一串 username 每次都得从别处翻出来手敲，
// 而「这个月给某客户出了几张账单、成本多少利润多少」更是无处可查——
// 那需要把客户、账期、金额三者关联着存下来，客户表是这条链的起点。

// Customer 一个客户：名称 + 名下的业务库账号列表。
type Customer struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Usernames string    `json:"usernames"` // 原始文本，换行/逗号分隔，由 splitList 解析
	Note      string    `json:"note"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// TaskCount 该客户已沉淀的账单任务数。列表页用它做删除确认，
	// 让「删客户会连带删掉 N 条历史」这件事在动手前就看得见。
	TaskCount int `json:"taskCount"`
}

// UsernameList 把存储的文本切成账号列表，复用与导出日志同一个分隔符口径。
func (c Customer) UsernameList() []string {
	return SplitAccountList(c.Usernames)
}

// EnsureCustomerSchema 建表（幂等），启动时调用一次。
func EnsureCustomerSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS customers (
			id BIGSERIAL PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			usernames TEXT NOT NULL DEFAULT '',
			note TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 customers 表失败: %w", err)
	}
	// name 上的 UNIQUE 已自带索引，这里不再重复建。
	return nil
}

// ListCustomers 客户列表，附各自的账单任务数（用于删除确认）。
//
// 用 LEFT JOIN + COUNT 一次查完，而不是每个客户再查一次任务数——
// 那会变成 N+1 次查询。GROUP BY c.id 在 PG 里可以带上同表其余列。
func ListCustomers(cfg PGConfig) ([]Customer, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT c.id, c.name, c.usernames, c.note, c.created_at, c.updated_at,
		       COUNT(t.id) AS task_count
		FROM customers c
		LEFT JOIN bill_export_tasks t ON t.customer_id = c.id
		GROUP BY c.id, c.name, c.usernames, c.note, c.created_at, c.updated_at
		ORDER BY c.name
	`)
	if err != nil {
		return nil, fmt.Errorf("查询客户列表失败: %w", err)
	}
	defer rows.Close()

	out := []Customer{}
	for rows.Next() {
		var c Customer
		if err := rows.Scan(&c.ID, &c.Name, &c.Usernames, &c.Note,
			&c.CreatedAt, &c.UpdatedAt, &c.TaskCount); err != nil {
			return nil, fmt.Errorf("读取客户列表失败: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取客户列表失败: %w", err)
	}
	return out, nil
}

// GetCustomer 按 ID 取单个客户。不存在时返回 (Customer{}, sql.ErrNoRows)。
func GetCustomer(cfg PGConfig, id int64) (Customer, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return Customer{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	var c Customer
	err = db.QueryRow(`
		SELECT id, name, usernames, note, created_at, updated_at
		FROM customers WHERE id = $1
	`, id).Scan(&c.ID, &c.Name, &c.Usernames, &c.Note, &c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return Customer{}, err
	}
	return c, nil
}

// UpsertCustomer 新增或更新客户。ID 为 0 表示新增，否则按 ID 更新。
//
// 按名称去重：名称是 UNIQUE，改成一个已被占用的名字会报错——
// 这里把它翻译成人话，而不是把 PG 的约束名抛给用户。
func UpsertCustomer(cfg PGConfig, c Customer) (Customer, error) {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return Customer{}, fmt.Errorf("客户名称不能为空")
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return Customer{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if c.ID == 0 {
		err = db.QueryRow(`
			INSERT INTO customers (name, usernames, note)
			VALUES ($1, $2, $3)
			RETURNING id, created_at, updated_at
		`, name, c.Usernames, c.Note).Scan(&c.ID, &c.CreatedAt, &c.UpdatedAt)
	} else {
		err = db.QueryRow(`
			UPDATE customers
			SET name = $2, usernames = $3, note = $4, updated_at = now()
			WHERE id = $1
			RETURNING created_at, updated_at
		`, c.ID, name, c.Usernames, c.Note).Scan(&c.CreatedAt, &c.UpdatedAt)
	}
	if err != nil {
		if strings.Contains(err.Error(), "duplicate key") {
			return Customer{}, fmt.Errorf("客户名称「%s」已存在", name)
		}
		return Customer{}, fmt.Errorf("保存客户失败: %w", err)
	}

	c.Name = name
	return c, nil
}

// DeleteCustomer 删客户。账单任务表上有 ON DELETE CASCADE，历史记录会一并删除。
//
// 这是**故意**的设计：客户删了，他的月度统计就没有归属，留着只会让汇总里出现
// 对不上人的数字。页面在调用前必须用 TaskCount 显示会连带删掉几条。
func DeleteCustomer(cfg PGConfig, id int64) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM customers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除客户失败: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除客户失败: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("客户不存在（可能已被删除）")
	}
	return nil
}

// SplitAccountList 把多行文本切成账号列表：换行、逗号、分号、空白都算分隔符，
// 去重且保序。
//
// 与 main.go 的 splitList 同一套口径，但放在 billing 里是为了让客户表的
// 「存下来的文本」与「导出日志要用的账号数组」用**同一个解析函数**——
// 两处各写一份，迟早出现「保存时切出 3 个账号、导出时切出 2 个」这种对不上的事。
func SplitAccountList(raw string) []string {
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		switch r {
		case '\n', '\r', ',', '，', ';', '；', '\t', ' ':
			return true
		}
		return false
	})
	seen := map[string]bool{}
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}
