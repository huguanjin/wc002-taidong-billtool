package billing

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// 一键出账任务的默认参数。
//
// 为什么要存起来：出账要指定单价来源、汇率、折扣、国产模型标识这一堆东西，
// 每次执行任务都让用户重填一遍，就等于把「一键」变成「五步」。
// 这些是**长期稳定**的商务口径（汇率 7、报价表折扣、哪些是国产模型），
// 不是每次变动的参数，所以存一份全局默认值，任务页只留客户 + 账期两个输入。

// BillTaskSettings 全局默认出账参数（单行表）。
type BillTaskSettings struct {
	PriceSource          string    `json:"priceSource"`          // official | price_table | db
	ExchangeRate         float64   `json:"exchangeRate"`         //
	Discount             *float64  `json:"discount"`             // nil = 按分组自动反推
	DomesticMarkers      string    `json:"domesticMarkers"`      // 原始文本，换行分隔
	SanitizedFormat      string    `json:"sanitizedFormat"`      // xlsx | csv | tsv
	IncludeBillingParams bool      `json:"includeBillingParams"` //
	UpdatedAt            time.Time `json:"updatedAt"`
}

// DomesticMarkerList 解析国产模型标识，复用与出账表单同一个分隔口径。
func (s BillTaskSettings) DomesticMarkerList() []string {
	return ParseMarkerList(s.DomesticMarkers)
}

// DefaultBillTaskSettings 表里还没有行时的默认值。
//
// 与出账页表单的默认值保持一致：单价来源 db（数据库实时价格）、汇率 7、
// 折扣不强制（nil，按分组反推）、脱敏日志 tsv。
func DefaultBillTaskSettings() BillTaskSettings {
	return BillTaskSettings{
		PriceSource:     string(PriceSourceDB),
		ExchangeRate:    DefaultExchangeRate,
		SanitizedFormat: "tsv",
	}
}

// EnsureSettingsSchema 建表（幂等），启动时调用一次。
func EnsureSettingsSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	// 单行表：CHECK (id = 1) 从数据库层面堵死「不小心插进第二行」，
	// 这样 GetSettings 可以放心地按 id=1 取，不必担心取到哪一行。
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS bill_task_settings (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			price_source TEXT NOT NULL DEFAULT 'db',
			exchange_rate DOUBLE PRECISION NOT NULL DEFAULT 7,
			discount DOUBLE PRECISION,
			domestic_markers TEXT NOT NULL DEFAULT '',
			sanitized_format TEXT NOT NULL DEFAULT 'tsv',
			include_billing_params BOOLEAN NOT NULL DEFAULT false,
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)
	`); err != nil {
		return fmt.Errorf("创建 bill_task_settings 表失败: %w", err)
	}
	return nil
}

// GetSettings 读默认参数。表里没有行时返回一套默认值（**不报错**）。
//
// 不报错是刻意的：首次部署时表是空的，若这里返回 error，
// 任务页一打开就是个红字错误，而用户其实什么都还没做错。
func GetSettings(cfg PGConfig) (BillTaskSettings, error) {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return BillTaskSettings{}, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	s := DefaultBillTaskSettings()
	var discount sql.NullFloat64
	var updatedAt time.Time
	err = db.QueryRow(`
		SELECT price_source, exchange_rate, discount, domestic_markers,
		       sanitized_format, include_billing_params, updated_at
		FROM bill_task_settings WHERE id = 1
	`).Scan(&s.PriceSource, &s.ExchangeRate, &discount, &s.DomesticMarkers,
		&s.SanitizedFormat, &s.IncludeBillingParams, &updatedAt)

	if err == sql.ErrNoRows {
		return DefaultBillTaskSettings(), nil
	}
	if err != nil {
		return BillTaskSettings{}, fmt.Errorf("读取默认出账参数失败: %w", err)
	}

	if discount.Valid {
		v := discount.Float64
		s.Discount = &v
	}
	s.UpdatedAt = updatedAt
	return s, nil
}

// SaveSettings 覆盖保存默认参数（单行 UPSERT）。
func SaveSettings(cfg PGConfig, s BillTaskSettings) error {
	// 单价来源只认这三个值：写进一个拼错的值，出账时会静默退回内置官方价，
	// 而用户以为自己在用数据库实时价格——账单会整体算错且不报错。
	switch PriceSource(s.PriceSource) {
	case PriceSourceOfficial, PriceSourcePriceTable, PriceSourceDB:
	case "":
		s.PriceSource = string(PriceSourceDB)
	default:
		return fmt.Errorf("未知的单价来源 %q", s.PriceSource)
	}

	if s.ExchangeRate <= 0 {
		s.ExchangeRate = DefaultExchangeRate
	}
	if s.Discount != nil && *s.Discount <= 0 {
		return fmt.Errorf("折扣必须大于 0（留空表示按分组自动反推）")
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(`
		INSERT INTO bill_task_settings
			(id, price_source, exchange_rate, discount, domestic_markers,
			 sanitized_format, include_billing_params, updated_at)
		VALUES (1, $1, $2, $3, $4, $5, $6, now())
		ON CONFLICT (id) DO UPDATE SET
			price_source = EXCLUDED.price_source,
			exchange_rate = EXCLUDED.exchange_rate,
			discount = EXCLUDED.discount,
			domestic_markers = EXCLUDED.domestic_markers,
			sanitized_format = EXCLUDED.sanitized_format,
			include_billing_params = EXCLUDED.include_billing_params,
			updated_at = now()
	`, s.PriceSource, s.ExchangeRate, s.Discount, s.DomesticMarkers,
		s.SanitizedFormat, s.IncludeBillingParams)
	if err != nil {
		return fmt.Errorf("保存默认出账参数失败: %w", err)
	}
	return nil
}

// ParseMarkerList 解析国产模型标识：换行/逗号分隔，去重保序，空则返回 nil。
//
// 与 main.go 的 parseMarkers 同一口径；放在 billing 里是为了让
// 「PG 里存的文本」和「出账时传的 []string」用同一个解析函数。
func ParseMarkerList(raw string) []string {
	return SplitAccountList(raw)
}

// JoinMarkerList 把标识列表存回单列文本，一行一个。
func JoinMarkerList(items []string) string {
	return strings.Join(items, "\n")
}
