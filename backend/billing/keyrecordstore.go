package billing

import (
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

// KeyRecord 一条上 key 记录：某个台子下某个渠道的消耗与成本/结算。
//
// 台子地址、对接人存快照（同时记 StageID 便于回溯）：台子后来被改名或删除，
// 历史记录里的结算对象不应跟着变。金额类字段由前端按「消耗 × 单价、金额 ÷ 汇率」
// 预填，但允许手工改，所以后端只存不重算。
type KeyRecord struct {
	ID             int64     `json:"id"`
	StageID        int64     `json:"stageId"`
	StageAddress   string    `json:"stageAddress"`
	StageContact   string    `json:"stageContact"`
	ChannelID      string    `json:"channelId"`
	Supplier       string    `json:"supplier"`
	Consumption    float64   `json:"consumption"`    // 渠道最新消耗（刀）
	CostUnitPrice  float64   `json:"costUnitPrice"`  // 渠道成本单价（元/刀）
	CostCNY        float64   `json:"costCny"`        // 渠道成本金额（元）
	FxRate         float64   `json:"fxRate"`         // U 汇率（元/U）
	CostUSDT       float64   `json:"costUsdt"`       // 渠道成本金额（U）
	StageUnitPrice float64   `json:"stageUnitPrice"` // 台子渠道单价（元/刀）
	SettleCNY      float64   `json:"settleCny"`      // 台子结算金额（元）
	SettleUSDT     float64   `json:"settleUsdt"`     // 台子结算金额（U）
	Alive          string    `json:"alive"`
	Notes          string    `json:"notes"`
	CreatedAt      time.Time `json:"createdAt"`
	UpdatedAt      time.Time `json:"updatedAt"`
}

const keyRecordColumns = `id, stage_id, stage_address, stage_contact, channel_id, supplier,
	consumption::float8, cost_unit_price::float8, cost_cny::float8, fx_rate::float8,
	cost_usdt::float8, stage_unit_price::float8, settle_cny::float8, settle_usdt::float8,
	alive, notes, created_at, updated_at`

// ListKeyRecords 记录列表，新的在前。
func ListKeyRecords(cfg PGConfig) ([]KeyRecord, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT ` + keyRecordColumns + ` FROM key_records ORDER BY id DESC`)
	if err != nil {
		return nil, fmt.Errorf("查询上key记录失败: %w", err)
	}
	defer rows.Close()

	out := []KeyRecord{}
	for rows.Next() {
		var r KeyRecord
		if err := rows.Scan(&r.ID, &r.StageID, &r.StageAddress, &r.StageContact, &r.ChannelID, &r.Supplier,
			&r.Consumption, &r.CostUnitPrice, &r.CostCNY, &r.FxRate, &r.CostUSDT,
			&r.StageUnitPrice, &r.SettleCNY, &r.SettleUSDT,
			&r.Alive, &r.Notes, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("读取上key记录失败: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取上key记录失败: %w", err)
	}
	return out, nil
}

// UpsertKeyRecord 新增或更新记录。ID 为 0 表示新增。
func UpsertKeyRecord(cfg PGConfig, r KeyRecord) (KeyRecord, error) {
	r.StageAddress = strings.TrimSpace(r.StageAddress)
	r.StageContact = strings.TrimSpace(r.StageContact)
	r.ChannelID = strings.TrimSpace(r.ChannelID)
	r.Supplier = strings.TrimSpace(r.Supplier)
	r.Alive = strings.TrimSpace(r.Alive)
	r.Notes = strings.TrimSpace(r.Notes)
	if r.StageAddress == "" {
		return KeyRecord{}, fmt.Errorf("台子地址不能为空")
	}

	nums := []struct {
		name string
		v    float64
	}{
		{"渠道最新消耗", r.Consumption}, {"渠道成本单价", r.CostUnitPrice}, {"渠道成本金额（元）", r.CostCNY},
		{"U汇率", r.FxRate}, {"渠道成本金额（U）", r.CostUSDT}, {"台子渠道单价", r.StageUnitPrice},
		{"台子结算金额（元）", r.SettleCNY}, {"台子结算金额（U）", r.SettleUSDT},
	}
	for _, n := range nums {
		if math.IsNaN(n.v) || math.IsInf(n.v, 0) || n.v < 0 {
			return KeyRecord{}, fmt.Errorf("%s不能为负数或非法数值", n.name)
		}
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return KeyRecord{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	args := []interface{}{
		r.StageID, r.StageAddress, r.StageContact, r.ChannelID, r.Supplier,
		r.Consumption, r.CostUnitPrice, r.CostCNY, r.FxRate, r.CostUSDT,
		r.StageUnitPrice, r.SettleCNY, r.SettleUSDT, r.Alive, r.Notes,
	}
	if r.ID == 0 {
		err = db.QueryRow(`
			INSERT INTO key_records
				(stage_id, stage_address, stage_contact, channel_id, supplier,
				 consumption, cost_unit_price, cost_cny, fx_rate, cost_usdt,
				 stage_unit_price, settle_cny, settle_usdt, alive, notes)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
			RETURNING id, created_at, updated_at
		`, args...).Scan(&r.ID, &r.CreatedAt, &r.UpdatedAt)
	} else {
		err = db.QueryRow(`
			UPDATE key_records
			SET stage_id=$2, stage_address=$3, stage_contact=$4, channel_id=$5, supplier=$6,
			    consumption=$7, cost_unit_price=$8, cost_cny=$9, fx_rate=$10, cost_usdt=$11,
			    stage_unit_price=$12, settle_cny=$13, settle_usdt=$14, alive=$15, notes=$16,
			    updated_at=now()
			WHERE id=$1
			RETURNING created_at, updated_at
		`, append([]interface{}{r.ID}, args...)...).Scan(&r.CreatedAt, &r.UpdatedAt)
		if err == sql.ErrNoRows {
			return KeyRecord{}, fmt.Errorf("记录不存在（可能已被删除）")
		}
	}
	if err != nil {
		return KeyRecord{}, fmt.Errorf("保存上key记录失败: %w", err)
	}
	return r, nil
}

// DeleteKeyRecord 删除记录。
func DeleteKeyRecord(cfg PGConfig, id int64) error {
	return deleteKeyRow(cfg, "key_records", "记录", id)
}
