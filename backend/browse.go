package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var browseRoot string

// initBrowseRoot 确定服务器文件浏览/直读的沙箱根目录，默认与账单模板同目录，可通过 BILL_BROWSE_ROOT 覆盖。
func initBrowseRoot() {
	root := os.Getenv("BILL_BROWSE_ROOT")
	if root == "" {
		root = dataDir
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		log.Fatalf("无法解析 BILL_BROWSE_ROOT: %v", err)
	}
	browseRoot = abs
	log.Printf("服务器文件浏览根目录: %s", browseRoot)
}

// resolveInBrowseRoot 将用户传入的相对/绝对路径限制在 browseRoot 内，防止目录穿越读取任意文件。
func resolveInBrowseRoot(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" || p == "." {
		return browseRoot, nil
	}

	var candidate string
	if filepath.IsAbs(p) {
		candidate = filepath.Clean(p)
	} else {
		candidate = filepath.Clean(filepath.Join(browseRoot, p))
	}

	rel, err := filepath.Rel(browseRoot, candidate)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径超出允许范围（根目录: %s）", browseRoot)
	}
	return candidate, nil
}

func isAllowedLogExt(ext string) bool {
	switch ext {
	case ".xlsx", ".csv", ".tsv":
		return true
	default:
		return false
	}
}

type browseEntry struct {
	Name    string `json:"name"`
	Path    string `json:"path"` // 相对 browseRoot 的路径，可直接回填到 serverPath
	IsDir   bool   `json:"isDir"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// handleBrowse 列出 browseRoot 内某个目录的子项，供前端可视化选择服务器上的源文件。
func handleBrowse(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	dirPath, err := resolveInBrowseRoot(r.URL.Query().Get("path"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err.Error())
		return
	}
	info, err := os.Stat(dirPath)
	if err != nil || !info.IsDir() {
		httpError(w, http.StatusBadRequest, "路径不存在或不是目录")
		return
	}

	entries, err := os.ReadDir(dirPath)
	if err != nil {
		httpError(w, http.StatusInternalServerError, err.Error())
		return
	}

	list := make([]browseEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && !isAllowedLogExt(strings.ToLower(filepath.Ext(e.Name()))) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		full := filepath.Join(dirPath, e.Name())
		rel, _ := filepath.Rel(browseRoot, full)
		list = append(list, browseEntry{
			Name:    e.Name(),
			Path:    filepath.ToSlash(rel),
			IsDir:   e.IsDir(),
			Size:    fi.Size(),
			ModTime: fi.ModTime().Format("2006-01-02 15:04:05"),
		})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDir != list[j].IsDir {
			return list[i].IsDir
		}
		return list[i].Name < list[j].Name
	})

	relCurrent, _ := filepath.Rel(browseRoot, dirPath)
	if relCurrent == "." {
		relCurrent = ""
	}
	parent := ""
	if dirPath != browseRoot {
		if p, err := filepath.Rel(browseRoot, filepath.Dir(dirPath)); err == nil && p != "." {
			parent = p
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"root":    browseRoot,
		"path":    filepath.ToSlash(relCurrent),
		"parent":  filepath.ToSlash(parent),
		"entries": list,
	})
}

// resolveInputFile 取得本次出账的输入文件：优先使用上传的 file 字段，否则使用服务器本地路径 serverPath 字段。
func resolveInputFile(r *http.Request, jobPath string) (string, error) {
	if file, header, err := r.FormFile("file"); err == nil {
		defer file.Close()
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !isAllowedLogExt(ext) {
			return "", fmt.Errorf("仅支持 .xlsx / .csv / .tsv 日志文件")
		}
		// 保留原始文件名（仅取 base name，避免路径穿越），以便账期/输出文件名从中推断。
		originalName := filepath.Base(header.Filename)
		if originalName == "" || originalName == "." || originalName == string(filepath.Separator) {
			originalName = "input" + ext
		}
		dstPath := filepath.Join(jobPath, originalName)
		dst, err := os.Create(dstPath)
		if err != nil {
			return "", err
		}
		defer dst.Close()
		if _, err := io.Copy(dst, file); err != nil {
			return "", err
		}
		return dstPath, nil
	}

	serverPath := ""
	if r.MultipartForm != nil {
		serverPath = formValue(r.MultipartForm.Value, "serverPath")
	}
	if strings.TrimSpace(serverPath) == "" {
		return "", fmt.Errorf("缺少上传文件 file 或服务器路径 serverPath")
	}

	resolved, err := resolveInBrowseRoot(serverPath)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || info.IsDir() {
		return "", fmt.Errorf("服务器路径不存在或不是文件: %s", serverPath)
	}
	ext := strings.ToLower(filepath.Ext(resolved))
	if !isAllowedLogExt(ext) {
		return "", fmt.Errorf("仅支持 .xlsx / .csv / .tsv 日志文件")
	}
	dstPath := filepath.Join(jobPath, filepath.Base(resolved))
	if err := copyFile(resolved, dstPath); err != nil {
		return "", err
	}
	return dstPath, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
