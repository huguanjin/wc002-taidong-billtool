package billing

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

// 按「客户 + 分组」手工维护的折扣，存本地 PostgreSQL。
//
// 为什么需要它：有些客户的折扣是线下临时谈的，没来得及写进 new-api 的分组倍率。
// 出账时按倍率反推出来的折扣与商务报出去的折扣对不上，账单金额就是错的。
// 结算人员在这里补一个真值，出账时按 KeyGroup 命中并覆盖反推（见 DiscountOverrides）。

// GroupDiscountRow 一条已维护的手工折扣。
type GroupDiscountRow struct {
	GroupKey string  `json:"groupKey"`
	Discount float64 `json:"discount"`
	Note     string  `json:"note"`
}

// GroupDiscountInput 一次保存请求里的一项。
//
// Discount 用指针：nil 表示**撤销**这条手工折扣（回到自动反推），
// 0 是合法值（商务谈成免费）。照 ChannelWithRatio.UpstreamRatio 的做法，
// 不让「未维护」和「配成 0」共用一个零值。
type GroupDiscountInput struct {
	GroupKey string   `json:"groupKey"`
	Discount *float64 `json:"discount"`
	Note     string   `json:"note"`
}

// EnsureCustomerGroupDiscountSchema 建表（幂等），启动时调用一次。
//
// 有外键指向 customers，必须在 EnsureCustomerSchema 之后调用。
func EnsureCustomerGroupDiscountSchema(cfg PGConfig) error {
	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	_, err = db.Exec(`
	CREATE TABLE IF NOT EXISTS customer_group_discounts (
		customer_id BIGINT NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
		group_key   TEXT NOT NULL,
		discount    DOUBLE PRECISION NOT NULL,
		note        TEXT NOT NULL DEFAULT '',
		updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
		PRIMARY KEY (customer_id, group_key)
	)`)
	if err != nil {
		return err
	}
	// 复合主键自带索引，不再重复建（customers 表同样的处理）。
	return nil
}

// UpsertCustomerGroupDiscounts 覆盖式保存一个客户的手工折扣。
//
// 语义是「保存后库里就是提交的这份」：提交里 Discount 为 nil 的分组会被删掉。
// 页面是整表提交的，若只做 upsert 不做删除，用户清空某个输入框后那条记录还在，
// 下次出账仍然按旧折扣算，而页面上显示的是空的——这种不一致极难排查。
func UpsertCustomerGroupDiscounts(cfg PGConfig, customerID int64, items []GroupDiscountInput) error {
	if customerID <= 0 {
		return fmt.Errorf("缺少客户 ID")
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

	// 先清空该客户的全部手工折扣，再写回提交的内容。
	// 比「逐条判断是删是留」简单，且不会漏删——一个客户的分组通常只有几十个。
	if _, err := tx.Exec(`DELETE FROM customer_group_discounts WHERE customer_id = $1`, customerID); err != nil {
		return fmt.Errorf("清理该客户已有手工折扣失败: %w", err)
	}

	stmt, err := tx.Prepare(`
		INSERT INTO customer_group_discounts (customer_id, group_key, discount, note, updated_at)
		VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (customer_id, group_key) DO UPDATE SET
			discount = EXCLUDED.discount,
			note = EXCLUDED.note,
			updated_at = now()`)
	if err != nil {
		return fmt.Errorf("准备写入语句失败: %w", err)
	}
	defer stmt.Close()

	for _, it := range items {
		group := strings.TrimSpace(it.GroupKey)
		if group == "" {
			return fmt.Errorf("分组名不能为空")
		}
		if it.Discount == nil {
			// nil = 撤销该分组的手工折扣，上面的整表删除已经覆盖，跳过即可。
			continue
		}
		if *it.Discount <= 0 || *it.Discount > 1 {
			return fmt.Errorf("分组「%s」的折扣应在 0 到 1 之间（如 6折 填 0.6），实际 %v", group, *it.Discount)
		}
		if _, err := stmt.Exec(customerID, group, *it.Discount, it.Note); err != nil {
			return fmt.Errorf("写入分组「%s」的折扣失败: %w", group, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交手工折扣失败: %w", err)
	}
	return nil
}

// ListCustomerGroupDiscounts 读一个客户已维护的手工折扣，按分组名排序。
func ListCustomerGroupDiscounts(cfg PGConfig, customerID int64) ([]GroupDiscountRow, error) {
	rows := []GroupDiscountRow{}
	if customerID <= 0 {
		return rows, nil
	}

	db, err := sql.Open("pgx", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接 PostgreSQL 失败: %w", err)
	}
	defer db.Close()

	result, err := db.Query(`
		SELECT group_key, discount, note
		FROM customer_group_discounts
		WHERE customer_id = $1
		ORDER BY group_key`, customerID)
	if err != nil {
		return nil, fmt.Errorf("读取手工折扣失败: %w", err)
	}
	defer result.Close()

	for result.Next() {
		var r GroupDiscountRow
		if err := result.Scan(&r.GroupKey, &r.Discount, &r.Note); err != nil {
			return nil, fmt.Errorf("读取手工折扣数据失败: %w", err)
		}
		rows = append(rows, r)
	}
	return rows, result.Err()
}

// CustomerGroupDiscountMap 出账时读：分组名 → 手工折扣。
//
// 返回的 map 直接喂给 ComputeGroupDiscounts（见 DiscountOverrides.Manual）。
func CustomerGroupDiscountMap(cfg PGConfig, customerID int64) (map[string]float64, error) {
	list, err := ListCustomerGroupDiscounts(cfg, customerID)
	if err != nil {
		return nil, err
	}
	out := make(map[string]float64, len(list))
	for _, r := range list {
		out[r.GroupKey] = r.Discount
	}
	return out, nil
}

// SortGroupDiscountRows 按分组名排序，供接口与页面稳定输出。
func SortGroupDiscountRows(rows []GroupDiscountRow) {
	sort.Slice(rows, func(i, j int) bool { return rows[i].GroupKey < rows[j].GroupKey })
}
