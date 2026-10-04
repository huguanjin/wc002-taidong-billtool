package billing

import (
	"encoding/csv"
	"os"
)

// CSVSanitizedWriter 流式写出脱敏日志为纯文本 csv/tsv 格式，没有 xlsx 单 sheet 的行数上限，
// 适合源日志有上千万行、xlsx 写不下的场景。
type CSVSanitizedWriter struct {
	f              *os.File
	w              *csv.Writer
	headers        []string
	includeBilling bool
	totalRows      int
	path           string
}

func NewCSVSanitizedWriter(path string, headers []string, delimiter rune, includeBilling bool) (*CSVSanitizedWriter, error) {
	sanitizedHeaders := buildSanitizedHeaders(headers, includeBilling)
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := csv.NewWriter(f)
	w.Comma = delimiter
	if err := w.Write(sanitizedHeaders); err != nil {
		f.Close()
		return nil, err
	}
	return &CSVSanitizedWriter{f: f, w: w, headers: headers, includeBilling: includeBilling, path: path}, nil
}

// WriteRow 实现 SanitizedRowWriter。
func (w *CSVSanitizedWriter) WriteRow(row []string, cacheRead, cacheWrite5m, cacheWrite1h float64, details RowDetails) error {
	values := make([]string, 0, len(w.headers)+4+len(SanitizedDetailColumns)+len(SanitizedBillingColumns))
	for i, name := range w.headers {
		if name == "" || SanitizedDropColumns[name] {
			continue
		}
		values = append(values, cellAt(row, i))
	}
	values = append(values,
		formatFloat(cacheRead),
		formatFloat(cacheWrite5m+cacheWrite1h),
		formatFloat(cacheWrite5m),
		formatFloat(cacheWrite1h),
	)
	for _, c := range detailCells(details) {
		values = append(values, cellToString(c))
	}
	if w.includeBilling {
		for _, c := range billingCells(details.Billing) {
			values = append(values, cellToString(c))
		}
	}
	w.totalRows++
	return w.w.Write(values)
}

// cellToString 把 detailCells/billingCells 的 interface{} 值转成 csv 单元格文本：
// nil 写空串，float64 走 formatFloat，string 原样写出。
func cellToString(v interface{}) string {
	switch t := v.(type) {
	case nil:
		return ""
	case float64:
		return formatFloat(t)
	case string:
		return t
	default:
		return ""
	}
}

func (w *CSVSanitizedWriter) RowsWritten() int { return w.totalRows }

func (w *CSVSanitizedWriter) Close() error {
	w.w.Flush()
	if err := w.w.Error(); err != nil {
		w.f.Close()
		return err
	}
	return w.f.Close()
}
