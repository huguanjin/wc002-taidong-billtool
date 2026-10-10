package billing

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 上游计费方式的维护表：(渠道, 模型) → 按量 / 按次（及单次费用）。
// 只在任务勾了「严格区分按次计费」时才被读取，见 percallcost.go。

// ChannelModelBillingRow 一条已维护的上游计费方式。
type ChannelModelBillingRow struct {
	ChannelID  int       `json:"channelId"`
	Model      string    `json:"model"`
	Mode       string    `json:"mode"`
	PerCallCNY float64   `json:"perCallCny"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// ChannelModelBillingInput 一次保存请求里的一项。
type ChannelModelBillingInput struct {
	ChannelID int    `json:"channelId"`
	Model     string `json:"model"`
	Mode      string `json:"mode"`
	// PerCallCNY 按次时必填且 > 0；按量时忽略（存 0）。
	PerCallCNY float64 `json:"perCallCny"`
}

// Validate 校验单项。按次的单次费用必须为正：0 会被读成「上游免费」，成本虚低且不报错。
func (in ChannelModelBillingInput) Validate() error {
	if in.ChannelID <= 0 {
		return fmt.Errorf("渠道号无效")
	}
	if strings.TrimSpace(in.Model) == "" {
		return fmt.Errorf("渠道 %d 缺少模型名", in.ChannelID)
	}
	switch in.Mode {
	case UpstreamModePerToken:
		return nil
	case UpstreamModePerCall:
		if !(in.PerCallCNY > 0) {
			return fmt.Errorf("渠道 %d 的模型 %s 选了按次计费，单次费用必须大于 0", in.ChannelID, in.Model)
		}
		return nil
	default:
		return fmt.Errorf("渠道 %d 的模型 %s 计费方式无效: %q", in.ChannelID, in.Model, in.Mode)
	}
}

// EnsureChannelModelBillingSchema 建表（幂等），启动时调用一次。
func EnsureChannelModelBillingSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	// 主键是 (渠道, 模型)：同一个模型在不同渠道，上游的收费方式可以不同。
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS channel_model_billing (
			channel_id   INTEGER NOT NULL,
			model_name   TEXT NOT NULL,
			mode         TEXT NOT NULL,
			per_call_cny DOUBLE PRECISION NOT NULL DEFAULT 0,
			updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
			PRIMARY KEY (channel_id, model_name)
		)
	`); err != nil {
		return fmt.Errorf("创建 channel_model_billing 表失败: %w", err)
	}
	return nil
}

// UpsertChannelModelBilling 批量保存，单事务；任何一项校验不过整批不写。
func UpsertChannelModelBilling(cfg PGConfig, items []ChannelModelBillingInput) error {
	for _, it := range items {
		if err := it.Validate(); err != nil {
			return err
		}
	}
	if len(items) == 0 {
		return nil
	}
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开启事务失败: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO channel_model_billing (channel_id, model_name, mode, per_call_cny, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (channel_id, model_name) DO UPDATE SET
			mode = EXCLUDED.mode,
			per_call_cny = EXCLUDED.per_call_cny,
			updated_at = now()
	`)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, it := range items {
		fee := it.PerCallCNY
		if it.Mode != UpstreamModePerCall {
			fee = 0
		}
		if _, err := stmt.Exec(it.ChannelID, strings.TrimSpace(it.Model), it.Mode, fee); err != nil {
			return fmt.Errorf("写入渠道 %d 模型 %s 失败: %w", it.ChannelID, it.Model, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交写入失败: %w", err)
	}
	return nil
}

// DeleteChannelModelBilling 删除一条（回到「未维护」，下次严格出账会再拦下让人补）。
func DeleteChannelModelBilling(cfg PGConfig, channelID int, model string) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()
	_, err = db.Exec(`DELETE FROM channel_model_billing WHERE channel_id = $1 AND model_name = $2`,
		channelID, strings.TrimSpace(model))
	return err
}

// ListChannelModelBilling 读出全部已维护项（渠道号、模型名升序）。
func ListChannelModelBilling(cfg PGConfig) ([]ChannelModelBillingRow, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT channel_id, model_name, mode, per_call_cny, updated_at
		FROM channel_model_billing ORDER BY channel_id, model_name`)
	if err != nil {
		return nil, fmt.Errorf("查询上游计费方式失败: %w", err)
	}
	defer rows.Close()

	out := []ChannelModelBillingRow{}
	for rows.Next() {
		var r ChannelModelBillingRow
		if err := rows.Scan(&r.ChannelID, &r.Model, &r.Mode, &r.PerCallCNY, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("读取上游计费方式失败: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ChannelModelBillingMap 读成出账用的映射。
//
// 库里 mode 不是已知取值的行（被人直接改过库）会被丢弃而不是当按量：
// 丢弃后该组合回到「未维护」，出账会被拦下；当按量则是静默按倍率估，
// 正是这个功能要避免的错。
func ChannelModelBillingMap(cfg PGConfig) (map[ChannelModelKey]UpstreamBilling, error) {
	list, err := ListChannelModelBilling(cfg)
	if err != nil {
		return nil, err
	}
	m := make(map[ChannelModelKey]UpstreamBilling, len(list))
	for _, r := range list {
		if r.Mode != UpstreamModePerCall && r.Mode != UpstreamModePerToken {
			continue
		}
		if r.Mode == UpstreamModePerCall && !(r.PerCallCNY > 0) {
			continue
		}
		m[ChannelModelKey{ChannelID: r.ChannelID, Model: r.Model}] = UpstreamBilling{
			Mode: r.Mode, PerCallCNY: r.PerCallCNY,
		}
	}
	return m, nil
}
