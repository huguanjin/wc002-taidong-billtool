package main

import (
	"net/http"

	"taidong-bill-backend/billing"
)

// ============================================================
// 上 key 资源整理：台子管理 + 上 key 货源
//
// 与客户信息一组接口同一套约定：GET 列表，POST 新增/更新，POST delete-* 删除，
// 全部 JSON body。
// ============================================================

// handleKeyStages 台子：GET 列表，POST 新增或更新。
func handleKeyStages(w http.ResponseWriter, r *http.Request) {
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），上 key 资源整理不可用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		stages, err := billing.ListKeyStages(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"stages": stages})

	case http.MethodPost:
		var in billing.KeyStage
		if !decodeJSONBody(w, r, &in) {
			return
		}
		saved, err := billing.UpsertKeyStage(*pgConfig, in)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"stage": saved})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDeleteKeyStage 删除台子。
func handleDeleteKeyStage(w http.ResponseWriter, r *http.Request) {
	handleDeleteKeyRow(w, r, "台子", billing.DeleteKeyStage)
}

// handleKeySources 货源：GET 列表，POST 新增或更新。
func handleKeySources(w http.ResponseWriter, r *http.Request) {
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*），上 key 资源整理不可用")
		return
	}
	switch r.Method {
	case http.MethodGet:
		sources, err := billing.ListKeySources(*pgConfig)
		if err != nil {
			httpError(w, http.StatusBadGateway, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"sources": sources})

	case http.MethodPost:
		var in billing.KeySource
		if !decodeJSONBody(w, r, &in) {
			return
		}
		saved, err := billing.UpsertKeySource(*pgConfig, in)
		if err != nil {
			httpError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"source": saved})

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleDeleteKeySource 删除货源。
func handleDeleteKeySource(w http.ResponseWriter, r *http.Request) {
	handleDeleteKeyRow(w, r, "货源", billing.DeleteKeySource)
}

func handleDeleteKeyRow(w http.ResponseWriter, r *http.Request, label string, del func(billing.PGConfig, int64) error) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if pgConfig == nil {
		httpError(w, http.StatusBadRequest, "未配置 PostgreSQL（BILL_PG_*）")
		return
	}
	var in struct {
		ID int64 `json:"id"`
	}
	if !decodeJSONBody(w, r, &in) {
		return
	}
	if in.ID <= 0 {
		httpError(w, http.StatusBadRequest, "缺少"+label+" ID")
		return
	}
	if err := del(*pgConfig, in.ID); err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": in.ID})
}
