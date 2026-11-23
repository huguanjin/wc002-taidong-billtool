package billing

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// 验证资源类别与台子多选绑定的 SQL：JSONB 读写、按类别统计台子数、
// 删除类别时同步摘掉台子上的绑定。需要真实 PostgreSQL，见 pg_integration_test.go 的说明。
func TestPGKeyCategoryBinding(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureKeyResourceSchema(cfg))
	// 建表必须幂等：启动时每次都会调用。
	require.NoError(t, EnsureKeyResourceSchema(cfg))

	a, err := UpsertKeyCategory(cfg, KeyCategory{Name: "GPT", Description: "openai"})
	require.NoError(t, err)
	b, err := UpsertKeyCategory(cfg, KeyCategory{Name: "Claude"})
	require.NoError(t, err)
	_, err = UpsertKeyCategory(cfg, KeyCategory{Name: "GPT"})
	require.ErrorContains(t, err, "已存在")

	s1, err := UpsertKeyStage(cfg, KeyStage{Address: "https://a", CategoryIDs: []int64{a.ID, b.ID, a.ID, 0}})
	require.NoError(t, err)
	require.Equal(t, []int64{a.ID, b.ID}, s1.CategoryIDs, "重复与非法 ID 应被清掉")
	s2, err := UpsertKeyStage(cfg, KeyStage{Address: "https://b", CategoryIDs: []int64{a.ID}})
	require.NoError(t, err)
	_, err = UpsertKeyStage(cfg, KeyStage{Address: "https://c"})
	require.NoError(t, err)

	cats, err := ListKeyCategories(cfg)
	require.NoError(t, err)
	require.Len(t, cats, 2)
	require.Equal(t, 2, cats[0].StageCount)
	require.Equal(t, 1, cats[1].StageCount)

	require.NoError(t, DeleteKeyCategory(cfg, a.ID))
	require.ErrorContains(t, DeleteKeyCategory(cfg, a.ID), "不存在")

	stages, err := ListKeyStages(cfg)
	require.NoError(t, err)
	byID := map[int64]KeyStage{}
	for _, s := range stages {
		byID[s.ID] = s
	}
	require.Equal(t, []int64{b.ID}, byID[s1.ID].CategoryIDs)
	require.Equal(t, []int64{}, byID[s2.ID].CategoryIDs)
}

// 上 key 记录的数值读写：NUMERIC 经 ::float8 读回，编辑与删除走通。
func TestPGKeyRecordRoundTrip(t *testing.T) {
	cfg := testPG(t)
	require.NoError(t, EnsureKeyResourceSchema(cfg))

	in := KeyRecord{
		StageID: 1, StageAddress: "https://a", StageContact: "张三", ChannelID: "858", Supplier: "供应商",
		Consumption: 120.5, CostUnitPrice: 2.1, CostCNY: 253.05, FxRate: 7.2, CostUSDT: 35.1458,
		StageUnitPrice: 3.2, SettleCNY: 385.6, SettleUSDT: 53.5556, Alive: "存活", Notes: "n",
	}
	saved, err := UpsertKeyRecord(cfg, in)
	require.NoError(t, err)
	require.Greater(t, saved.ID, int64(0))

	saved.Alive = "已封"
	saved.SettleCNY = 400
	_, err = UpsertKeyRecord(cfg, saved)
	require.NoError(t, err)

	list, err := ListKeyRecords(cfg)
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "已封", list[0].Alive)
	require.InDelta(t, 400, list[0].SettleCNY, 1e-9)
	require.InDelta(t, 120.5, list[0].Consumption, 1e-9)
	require.InDelta(t, 35.1458, list[0].CostUSDT, 1e-9)

	_, err = UpsertKeyRecord(cfg, KeyRecord{StageAddress: "x", Consumption: -1})
	require.Error(t, err)

	require.NoError(t, DeleteKeyRecord(cfg, saved.ID))
	require.ErrorContains(t, DeleteKeyRecord(cfg, saved.ID), "不存在")
}
