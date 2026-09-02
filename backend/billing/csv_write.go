package billing

import (
	"encoding/csv"
	"os"
)

// CSVSanitizedWriter 流式写出脱敏日志为纯文本 csv/tsv 格式，没有 xlsx 单 sheet 的行数上限，
// 适合源日志有上千万行、xlsx 写不下的场景。
type CSVSanitizedWriter struct {
	f         *os.File
	w         *csv.Writer
	headers   []string
	totalRows int
	path      string
}

func NewCSVSanitizedWriter(path string, headers []string, delimiter rune) (*CSVSanitizedWriter, error) {
	sanitizedHeaders := buildSanitizedHeaders(headers)
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
	return &CSVSanitizedWriter{f: f, w: w, headers: headers, path: path}, nil
}

// WriteRow 实现 SanitizedRowWriter。
func (w *CSVSanitizedWriter) WriteRow(row []string, cacheRead, cacheWrite5m, cacheWrite1h float64) error {
	values := make([]string, 0, len(w.headers)+4)
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
	w.totalRows++
	return w.w.Write(values)
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
