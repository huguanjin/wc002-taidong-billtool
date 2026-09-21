package billing

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// 倍率解析命中的规则层级，对应 group_ratio_source.md 里「倍率解析算法」的三个优先级分支。
const (
	DerivedFromGroupGroupRatio = "group_group_ratio"
	DerivedFromGroupRatio      = "group_ratio"
	DerivedFromFallbackDefault = "fallback_default"
)

// UserDiscountResult 一次"拉取客户折扣"的结果快照。
type UserDiscountResult struct {
	UserID      int       `json:"userId"`
	Username    string    `json:"username"`
	UserGroup   string    `json:"userGroup"`
	GroupRatio  float64   `json:"groupRatio"`
	Discount    float64   `json:"discount"`
	DerivedFrom string    `json:"derivedFrom"`
	FetchedAt   time.Time `json:"fetchedAt"`
}

// FetchUserGroupDiscount 连业务 MySQL 库查一次指定用户的分组倍率，换算成折扣。
// userID、username 至少提供一个；两者都提供时按 "id = ? OR username = ?" 匹配。
func FetchUserGroupDiscount(cfg DBConfig, userID *int, username string) (*UserDiscountResult, error) {
	db, err := sql.Open("mysql", cfg.dsn())
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}
	defer db.Close()

	idArg := -1
	if userID != nil {
		idArg = *userID
	}

	var id int
	var name, userGroup string
	row := db.QueryRow(
		"SELECT id, username, `group` FROM users WHERE deleted_at IS NULL AND (id = ? OR username = ?) LIMIT 1",
		idArg, username,
	)
	if err := row.Scan(&id, &name, &userGroup); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("用户不存在（userId=%v, username=%q）", userID, username)
		}
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}

	groupRatio, derivedFrom, err := lookupGroupRatio(db, userGroup)
	if err != nil {
		return nil, err
	}

	return &UserDiscountResult{
		UserID:      id,
		Username:    name,
		UserGroup:   userGroup,
		GroupRatio:  groupRatio,
		Discount:    groupRatio / DiscountBaseFactor,
		DerivedFrom: derivedFrom,
		FetchedAt:   time.Now(),
	}, nil
}

// lookupGroupRatio 按 GetUserGroupRatio 的优先级（GroupGroupRatio[userGroup][userGroup] →
// GroupRatio[userGroup] → 兜底 1）算出用户实际适用的分组倍率。billtool 第一版不查 tokens.group，
// 「实际使用分组」直接取 userGroup 本身。
func lookupGroupRatio(db *sql.DB, userGroup string) (float64, string, error) {
	rows, err := db.Query("SELECT `key`, `value` FROM `options` WHERE `key` IN ('GroupRatio', 'GroupGroupRatio')")
	if err != nil {
		return 0, "", fmt.Errorf("查询 options 表失败: %w", err)
	}
	defer rows.Close()

	raw := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return 0, "", fmt.Errorf("读取 options 表数据失败: %w", err)
		}
		raw[key] = value
	}
	if err := rows.Err(); err != nil {
		return 0, "", fmt.Errorf("读取 options 表数据失败: %w", err)
	}

	var groupGroupRatio map[string]map[string]float64
	if v, ok := raw["GroupGroupRatio"]; ok {
		if err := json.Unmarshal([]byte(v), &groupGroupRatio); err != nil {
			return 0, "", fmt.Errorf("解析 GroupGroupRatio 失败: %w", err)
		}
	}
	if ratio, ok := groupGroupRatio[userGroup][userGroup]; ok {
		return ratio, DerivedFromGroupGroupRatio, nil
	}

	var groupRatio map[string]float64
	if v, ok := raw["GroupRatio"]; ok {
		if err := json.Unmarshal([]byte(v), &groupRatio); err != nil {
			return 0, "", fmt.Errorf("解析 GroupRatio 失败: %w", err)
		}
	}
	if ratio, ok := groupRatio[userGroup]; ok {
		return ratio, DerivedFromGroupRatio, nil
	}

	return 1, DerivedFromFallbackDefault, nil
}
