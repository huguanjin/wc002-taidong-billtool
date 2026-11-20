package billing

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// 上 key 资源整理：台子（下游收 key 的平台）与上 key 货源（号主手里的资源）两张表。
// 两张表互不关联，只是各自的台账，所以没有外键。

// KeyStageModel 台子的一条「需求模型 → 价格」。价格存文本：
// 实际报价常带单位或说明（如「0.8 元/刀」），强行转数字会丢信息。
type KeyStageModel struct {
	Model string `json:"model"`
	Price string `json:"price"`
}

// KeyStage 一个台子。ID 由数据库自增，作为台子编号展示。
type KeyStage struct {
	ID        int64           `json:"id"`
	Address   string          `json:"address"`
	Contact   string          `json:"contact"`
	Models    []KeyStageModel `json:"models"`
	Notes     string          `json:"notes"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// KeySource 一条上 key 货源。ID 即货源序号。
type KeySource struct {
	ID          int64     `json:"id"`
	AddedDate   string    `json:"addedDate"` // YYYY-MM-DD
	OwnerName   string    `json:"ownerName"`
	Resource    string    `json:"resourceName"`
	ConfigDesc  string    `json:"configDesc"`
	UnitPrice   string    `json:"unitPrice"`
	OwnerDemand string    `json:"ownerDemand"`
	Delisted    bool      `json:"delisted"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

const keyDateLayout = "2006-01-02"

// EnsureKeyResourceSchema 建表（幂等），启动时调用一次。
func EnsureKeyResourceSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS key_stages (
			id BIGSERIAL PRIMARY KEY,
			address TEXT NOT NULL DEFAULT '',
			contact TEXT NOT NULL DEFAULT '',
			models TEXT NOT NULL DEFAULT '[]',
			notes TEXT NOT NULL DEFAULT '',
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 key_stages 表失败: %w", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS key_sources (
			id BIGSERIAL PRIMARY KEY,
			added_date DATE NOT NULL DEFAULT CURRENT_DATE,
			owner_name TEXT NOT NULL DEFAULT '',
			resource_name TEXT NOT NULL DEFAULT '',
			config_desc TEXT NOT NULL DEFAULT '',
			unit_price TEXT NOT NULL DEFAULT '',
			owner_demand TEXT NOT NULL DEFAULT '',
			delisted BOOLEAN NOT NULL DEFAULT FALSE,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 key_sources 表失败: %w", err)
	}
	return nil
}

// cleanStageModels 去掉模型名为空的行并裁掉首尾空白；始终返回非 nil 切片，
// 保证 JSON 里是 [] 而不是 null。
func cleanStageModels(in []KeyStageModel) []KeyStageModel {
	out := make([]KeyStageModel, 0, len(in))
	for _, m := range in {
		model := strings.TrimSpace(m.Model)
		if model == "" {
			continue
		}
		out = append(out, KeyStageModel{Model: model, Price: strings.TrimSpace(m.Price)})
	}
	return out
}

// ListKeyStages 台子列表，按编号升序。
func ListKeyStages(cfg PGConfig) ([]KeyStage, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id, address, contact, models, notes, created_at, updated_at
		FROM key_stages ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("查询台子列表失败: %w", err)
	}
	defer rows.Close()

	out := []KeyStage{}
	for rows.Next() {
		var s KeyStage
		var models string
		if err := rows.Scan(&s.ID, &s.Address, &s.Contact, &models, &s.Notes, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("读取台子列表失败: %w", err)
		}
		// 解析失败按空列表处理：一行坏数据不应让整张台账打不开。
		if err := json.Unmarshal([]byte(models), &s.Models); err != nil || s.Models == nil {
			s.Models = []KeyStageModel{}
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取台子列表失败: %w", err)
	}
	return out, nil
}

// UpsertKeyStage 新增或更新台子。ID 为 0 表示新增。
func UpsertKeyStage(cfg PGConfig, s KeyStage) (KeyStage, error) {
	s.Address = strings.TrimSpace(s.Address)
	if s.Address == "" {
		return KeyStage{}, fmt.Errorf("台子地址不能为空")
	}
	s.Contact = strings.TrimSpace(s.Contact)
	s.Notes = strings.TrimSpace(s.Notes)
	s.Models = cleanStageModels(s.Models)
	raw, err := json.Marshal(s.Models)
	if err != nil {
		return KeyStage{}, fmt.Errorf("序列化需求模型失败: %w", err)
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return KeyStage{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if s.ID == 0 {
		err = db.QueryRow(`
			INSERT INTO key_stages (address, contact, models, notes)
			VALUES ($1, $2, $3, $4)
			RETURNING id, created_at, updated_at
		`, s.Address, s.Contact, string(raw), s.Notes).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
	} else {
		err = db.QueryRow(`
			UPDATE key_stages
			SET address = $2, contact = $3, models = $4, notes = $5, updated_at = now()
			WHERE id = $1
			RETURNING created_at, updated_at
		`, s.ID, s.Address, s.Contact, string(raw), s.Notes).Scan(&s.CreatedAt, &s.UpdatedAt)
		if err == sql.ErrNoRows {
			return KeyStage{}, fmt.Errorf("台子不存在（可能已被删除）")
		}
	}
	if err != nil {
		return KeyStage{}, fmt.Errorf("保存台子失败: %w", err)
	}
	return s, nil
}

// DeleteKeyStage 删除台子。
func DeleteKeyStage(cfg PGConfig, id int64) error {
	return deleteKeyRow(cfg, "key_stages", "台子", id)
}

// ListKeySources 货源列表，按序号升序。
func ListKeySources(cfg PGConfig) ([]KeySource, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT id, added_date, owner_name, resource_name, config_desc,
		       unit_price, owner_demand, delisted, created_at, updated_at
		FROM key_sources ORDER BY id
	`)
	if err != nil {
		return nil, fmt.Errorf("查询货源列表失败: %w", err)
	}
	defer rows.Close()

	out := []KeySource{}
	for rows.Next() {
		var s KeySource
		var added time.Time
		if err := rows.Scan(&s.ID, &added, &s.OwnerName, &s.Resource, &s.ConfigDesc,
			&s.UnitPrice, &s.OwnerDemand, &s.Delisted, &s.CreatedAt, &s.UpdatedAt); err != nil {
			return nil, fmt.Errorf("读取货源列表失败: %w", err)
		}
		s.AddedDate = added.Format(keyDateLayout)
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取货源列表失败: %w", err)
	}
	return out, nil
}

// UpsertKeySource 新增或更新货源。ID 为 0 表示新增；添加日期留空则取当天。
func UpsertKeySource(cfg PGConfig, s KeySource) (KeySource, error) {
	s.OwnerName = strings.TrimSpace(s.OwnerName)
	s.Resource = strings.TrimSpace(s.Resource)
	s.ConfigDesc = strings.TrimSpace(s.ConfigDesc)
	s.UnitPrice = strings.TrimSpace(s.UnitPrice)
	s.OwnerDemand = strings.TrimSpace(s.OwnerDemand)
	if s.OwnerName == "" {
		return KeySource{}, fmt.Errorf("号主名称不能为空")
	}
	if s.Resource == "" {
		return KeySource{}, fmt.Errorf("资源名称不能为空")
	}

	s.AddedDate = strings.TrimSpace(s.AddedDate)
	if s.AddedDate == "" {
		s.AddedDate = time.Now().Format(keyDateLayout)
	}
	added, err := time.Parse(keyDateLayout, s.AddedDate)
	if err != nil {
		return KeySource{}, fmt.Errorf("添加日期格式应为 YYYY-MM-DD")
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return KeySource{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	if s.ID == 0 {
		err = db.QueryRow(`
			INSERT INTO key_sources
				(added_date, owner_name, resource_name, config_desc, unit_price, owner_demand, delisted)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			RETURNING id, created_at, updated_at
		`, added, s.OwnerName, s.Resource, s.ConfigDesc, s.UnitPrice, s.OwnerDemand, s.Delisted,
		).Scan(&s.ID, &s.CreatedAt, &s.UpdatedAt)
	} else {
		err = db.QueryRow(`
			UPDATE key_sources
			SET added_date = $2, owner_name = $3, resource_name = $4, config_desc = $5,
			    unit_price = $6, owner_demand = $7, delisted = $8, updated_at = now()
			WHERE id = $1
			RETURNING created_at, updated_at
		`, s.ID, added, s.OwnerName, s.Resource, s.ConfigDesc, s.UnitPrice, s.OwnerDemand, s.Delisted,
		).Scan(&s.CreatedAt, &s.UpdatedAt)
		if err == sql.ErrNoRows {
			return KeySource{}, fmt.Errorf("货源不存在（可能已被删除）")
		}
	}
	if err != nil {
		return KeySource{}, fmt.Errorf("保存货源失败: %w", err)
	}
	return s, nil
}

// DeleteKeySource 删除货源。
func DeleteKeySource(cfg PGConfig, id int64) error {
	return deleteKeyRow(cfg, "key_sources", "货源", id)
}

// deleteKeyRow 按 ID 删一行。table 只来自上面两个调用点的字面量，不接受外部输入。
func deleteKeyRow(cfg PGConfig, table, label string, id int64) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	res, err := db.Exec(`DELETE FROM `+table+` WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("删除%s失败: %w", label, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("删除%s失败: %w", label, err)
	}
	if n == 0 {
		return fmt.Errorf("%s不存在（可能已被删除）", label)
	}
	return nil
}
