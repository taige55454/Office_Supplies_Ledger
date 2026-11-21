package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/getlantern/systray"
	"github.com/xuri/excelize/v2"
	_ "modernc.org/sqlite"
)

//go:embed web/*
var embeddedWebFS embed.FS

// ==================== 1. 全局配置与目录初始化 ====================

type AppConfig struct {
	Port          int    `json:"port"`
	AdminPasscode string `json:"admin_passcode"`
	EntryPasscode string `json:"entry_passcode"`
	AllowLan      bool   `json:"allow_lan"`
	SecretSalt    string `json:"secret_salt"`
}

var (
	dataRootDir   string
	dbPath        string
	configPath    string
	attachRootDir string
	backupDir     string

	appCfg      AppConfig
	cfgMutex    sync.RWMutex
	db          *sql.DB
	httpSrv     *http.Server
	srvMutex    sync.Mutex
	currentPort int
)

func resolveDataRoot() string {
	if envRoot := os.Getenv("LEDGER_DATA_ROOT"); envRoot != "" {
		return envRoot
	}
	exePath, err := os.Executable()
	if err == nil && !strings.Contains(exePath, "go-build") && !strings.Contains(exePath, "Temp") {
		return filepath.Join(filepath.Dir(exePath), "OfficeLedgerData")
	}
	cwd, _ := os.Getwd()
	return filepath.Join(cwd, "OfficeLedgerData")
}

func initDirectoriesAndConfig() error {
	dataRootDir = resolveDataRoot()
	dbPath = filepath.Join(dataRootDir, "database.sqlite")
	configPath = filepath.Join(dataRootDir, "config.json")
	attachRootDir = filepath.Join(dataRootDir, "attachments")
	backupDir = filepath.Join(dataRootDir, "backups")

	subDirs := []string{
		dataRootDir,
		attachRootDir,
		filepath.Join(attachRootDir, "1_消耗品月度手写登记表"),
		filepath.Join(attachRootDir, "2_复用品实物图片"),
		filepath.Join(attachRootDir, "3_复用品报废凭证"),
		filepath.Join(attachRootDir, "4_导出报表暂存"),
		backupDir,
	}
	for _, d := range subDirs {
		if err := os.MkdirAll(d, 0755); err != nil {
			return err
		}
	}

	if _, err := os.Stat(configPath); errors.Is(err, os.ErrNotExist) {
		saltBytes := make([]byte, 16)
		_, _ = rand.Read(saltBytes)
		appCfg = AppConfig{
			Port:          8088,
			AdminPasscode: "888888",
			EntryPasscode: "666666",
			AllowLan:      true,
			SecretSalt:    hex.EncodeToString(saltBytes),
		}
		return saveConfigToFile()
	}

	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &appCfg); err != nil {
		return err
	}
	if appCfg.Port <= 0 || appCfg.Port > 65535 {
		appCfg.Port = 8088
	}
	if appCfg.SecretSalt == "" {
		saltBytes := make([]byte, 16)
		_, _ = rand.Read(saltBytes)
		appCfg.SecretSalt = hex.EncodeToString(saltBytes)
		_ = saveConfigToFile()
	}
	return nil
}

func saveConfigToFile() error {
	cfgMutex.Lock()
	defer cfgMutex.Unlock()
	b, err := json.MarshalIndent(appCfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, b, 0644)
}

// ==================== 2. 数据库初始化与预置字典 ====================

func initDatabase() error {
	var err error
	db, err = sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // SQLite 单写连接避免并发锁冲突

	schema := `
	CREATE TABLE IF NOT EXISTS items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		category TEXT NOT NULL,
		name TEXT NOT NULL,
		nature TEXT NOT NULL CHECK(nature IN ('consumable', 'reusable')),
		base_unit TEXT NOT NULL,
		pack_unit TEXT NOT NULL DEFAULT '',
		pack_size REAL NOT NULL DEFAULT 1,
		min_price REAL NOT NULL DEFAULT 0,
		max_price REAL NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		UNIQUE(category, nature, name)
	);

	CREATE TABLE IF NOT EXISTS consumable_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ledger_year INTEGER NOT NULL,
		seq_num INTEGER NOT NULL,
		serial_no TEXT NOT NULL UNIQUE,
		item_id INTEGER NOT NULL,
		tx_type TEXT NOT NULL CHECK(tx_type IN ('IN', 'OUT')),
		tx_date TEXT NOT NULL,
		base_qty REAL NOT NULL,
		pack_qty REAL NOT NULL,
		total_amount REAL NOT NULL DEFAULT 0,
		unit_price REAL NOT NULL DEFAULT 0,
		recipient TEXT NOT NULL DEFAULT '',
		remark TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY(item_id) REFERENCES items(id) ON DELETE RESTRICT
	);
	CREATE INDEX IF NOT EXISTS idx_cl_item_date ON consumable_ledger(item_id, tx_date);
	CREATE INDEX IF NOT EXISTS idx_cl_year_seq ON consumable_ledger(ledger_year, seq_num);

	CREATE TABLE IF NOT EXISTS annual_carryover (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		carry_year INTEGER NOT NULL,
		item_id INTEGER NOT NULL,
		closing_base_qty REAL NOT NULL,
		created_at TEXT NOT NULL,
		UNIQUE(carry_year, item_id),
		FOREIGN KEY(item_id) REFERENCES items(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS reusable_assets (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		asset_no TEXT NOT NULL UNIQUE,
		item_id INTEGER NOT NULL,
		sub_name TEXT NOT NULL DEFAULT '',
		purchase_date TEXT NOT NULL,
		unit_price REAL NOT NULL DEFAULT 0,
		expected_years INTEGER NOT NULL DEFAULT 10,
		status TEXT NOT NULL DEFAULT '闲置' CHECK(status IN ('闲置', '在用', '报废')),
		current_user TEXT NOT NULL DEFAULT '',
		image_path TEXT NOT NULL DEFAULT '',
		scrap_date TEXT NOT NULL DEFAULT '',
		scrap_file_path TEXT NOT NULL DEFAULT '',
		remark TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY(item_id) REFERENCES items(id) ON DELETE RESTRICT
	);
	CREATE INDEX IF NOT EXISTS idx_ra_item ON reusable_assets(item_id);
	CREATE INDEX IF NOT EXISTS idx_ra_status ON reusable_assets(status);

	CREATE TABLE IF NOT EXISTS reusable_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		log_no TEXT NOT NULL UNIQUE,
		asset_id INTEGER NOT NULL,
		action_type TEXT NOT NULL,
		action_date TEXT NOT NULL,
		operator_or_user TEXT NOT NULL DEFAULT '',
		detail TEXT NOT NULL DEFAULT '',
		attachment_path TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		FOREIGN KEY(asset_id) REFERENCES reusable_assets(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS idx_rl_asset ON reusable_logs(asset_id);

	CREATE TABLE IF NOT EXISTS monthly_attachments (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		year_month TEXT NOT NULL,
		title TEXT NOT NULL,
		file_path TEXT NOT NULL,
		original_name TEXT NOT NULL,
		remark TEXT NOT NULL DEFAULT '',
		uploaded_at TEXT NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_ma_ym ON monthly_attachments(year_month);

	CREATE TABLE IF NOT EXISTS recipients (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		last_used_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS audit_logs (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		action TEXT NOT NULL,
		target_info TEXT NOT NULL,
		detail TEXT NOT NULL,
		created_at TEXT NOT NULL
	);
	`
	if _, err := db.Exec(schema); err != nil {
		return err
	}

	seedDefaultItems()
	autoDailyBackup()
	return nil
}

func seedDefaultItems() {
	var cnt int
	_ = db.QueryRow(`SELECT COUNT(1) FROM items`).Scan(&cnt)
	if cnt > 0 {
		return
	}
	now := nowStr()
	seeds := [][]interface{}{
		{"办公文具", "黑色中性笔", "consumable", "支", "盒", 12},
		{"办公文具", "A5线圈笔记本", "consumable", "本", "包", 10},
		{"办公文具", "长尾夹(中号)", "consumable", "个", "盒", 24},
		{"办公文具", "得力订书机", "reusable", "个", "盒", 1},
		{"办公文具", "桌面计算器", "reusable", "台", "盒", 1},
		{"办公耗材", "A4打印纸(70g)", "consumable", "包", "箱", 5},
		{"办公耗材", "A3打印纸(80g)", "consumable", "包", "箱", 5},
		{"办公耗材", "原装黑色硒鼓", "consumable", "支", "箱", 2},
		{"办公家具", "标准办公桌", "reusable", "张", "套", 1},
		{"办公家具", "人体工学办公椅", "reusable", "把", "箱", 1},
		{"办公家具", "双门文件柜", "reusable", "组", "件", 1},
	}
	stmt, err := db.Prepare(`INSERT OR IGNORE INTO items (category, name, nature, base_unit, pack_unit, pack_size, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return
	}
	defer stmt.Close()
	for _, s := range seeds {
		_, _ = stmt.Exec(s[0], s[1], s[2], s[3], s[4], s[5], now, now)
	}
}

func autoDailyBackup() {
	todayCompact := time.Now().Format("20060102")
	dest := filepath.Join(backupDir, fmt.Sprintf("db_%s.sqlite", todayCompact))
	if _, err := os.Stat(dest); err == nil {
		return
	}
	_, _ = db.Exec(fmt.Sprintf(`VACUUM INTO '%s'`, strings.ReplaceAll(dest, "'", "''")))
}

// ==================== 3. 通用工具函数与安全鉴权 ====================

func nowStr() string {
	return time.Now().Format("2006-01-02 15:04:05")
}

func todayStr() string {
	return time.Now().Format("2006-01-02")
}

func round2(v float64) float64 {
	return math.Round(v*100) / 100
}

func round4(v float64) float64 {
	return math.Round(v*10000) / 10000
}

func sanitizeToken(s string) string {
	s = strings.TrimSpace(s)
	replacer := strings.NewReplacer("\\", "_", "/", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_", " ", "_")
	res := replacer.Replace(s)
	if res == "" {
		return "FILE"
	}
	return res
}

func writeAudit(action, target, detail string) {
	_, _ = db.Exec(`INSERT INTO audit_logs (action, target_info, detail, created_at) VALUES (?, ?, ?, ?)`, action, target, detail, nowStr())
}

func recordRecipient(name string) {
	clean := strings.TrimSpace(name)
	if clean == "" {
		return
	}
	_, _ = db.Exec(`INSERT INTO recipients (name, last_used_at) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET last_used_at = excluded.last_used_at`, clean, nowStr())
	// 仅保留最新 10 条不重复领用人
	_, _ = db.Exec(`DELETE FROM recipients WHERE id NOT IN (SELECT id FROM recipients ORDER BY last_used_at DESC LIMIT 10)`)
}

func isLocalRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	return host == "127.0.0.1" || host == "::1"
}

func signRoleToken(role string) string {
	cfgMutex.RLock()
	salt := appCfg.SecretSalt
	cfgMutex.RUnlock()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	payload := role + "|" + ts
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(payload))
	sig := hex.EncodeToString(mac.Sum(nil))
	return payload + "|" + sig
}

func verifyRoleToken(token string) (string, bool) {
	parts := strings.Split(token, "|")
	if len(parts) != 3 {
		return "", false
	}
	role, tsStr, sig := parts[0], parts[1], parts[2]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil || time.Now().Unix()-ts > 16*3600 {
		return "", false
	}
	cfgMutex.RLock()
	salt := appCfg.SecretSalt
	cfgMutex.RUnlock()
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(role + "|" + tsStr))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(sig), []byte(expected)) {
		return "", false
	}
	return role, true
}

func getRequestRole(r *http.Request) string {
	if isLocalRequest(r) {
		return "admin"
	}
	tk := r.Header.Get("X-Auth-Token")
	if tk == "" {
		tk = r.URL.Query().Get("token")
	}
	if role, ok := verifyRoleToken(tk); ok {
		return role
	}
	return "guest"
}

func verifyAdminPasscode(pass string) bool {
	cfgMutex.RLock()
	target := appCfg.AdminPasscode
	cfgMutex.RUnlock()
	return strings.TrimSpace(pass) == strings.TrimSpace(target)
}

func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// ==================== 4. 权限拦截中间件 ====================

func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if !strings.HasPrefix(path, "/api/") && !strings.HasPrefix(path, "/attachments/") {
			next.ServeHTTP(w, r)
			return
		}
		if path == "/api/auth/status" || path == "/api/auth/login" {
			next.ServeHTTP(w, r)
			return
		}

		role := getRequestRole(r)
		if role == "guest" {
			cfgMutex.RLock()
			allowLan := appCfg.AllowLan
			cfgMutex.RUnlock()
			if !allowLan {
				writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "主机已关闭内网外部访问"})
				return
			}
			writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"success": false, "locked": true, "message": "请输入内网访问口令"})
			return
		}

		// 若为“内网录入员 (entry)”，严格禁止所有修改(PUT)、删除(DELETE)、年度结转及系统设置操作
		if role == "entry" {
			if r.Method == http.MethodPut || r.Method == http.MethodDelete ||
				strings.HasPrefix(path, "/api/settings") ||
				strings.HasPrefix(path, "/api/carryover") {
				writeJSON(w, http.StatusForbidden, map[string]interface{}{
					"success": false,
					"message": "当前为【内网登记员】只读/录入权限，无权修改、删除或执行结转！如需修改请联系管理员。",
				})
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// ==================== 5. 鉴权与系统设置接口 ====================

func getLanIPs() []string {
	var ips []string
	ifaces, err := net.Interfaces()
	if err != nil {
		return ips
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		nameLower := strings.ToLower(iface.Name)
		if strings.Contains(nameLower, "vmware") || strings.Contains(nameLower, "virtual") || strings.Contains(nameLower, "wsl") || strings.Contains(nameLower, "docker") {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipNet, ok := addr.(*net.IPNet); ok && !ipNet.IP.IsLoopback() && ipNet.IP.To4() != nil {
				ips = append(ips, ipNet.IP.String())
			}
		}
	}
	return ips
}

func handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	role := getRequestRole(r)
	isLocal := isLocalRequest(r)
	cfgMutex.RLock()
	cfgCopy := appCfg
	cfgMutex.RUnlock()

	port := currentPort
	if port == 0 {
		port = cfgCopy.Port
	}
	var urls []string
	for _, ip := range getLanIPs() {
		urls = append(urls, fmt.Sprintf("http://%s:%d", ip, port))
	}

	resp := map[string]interface{}{
		"success":      true,
		"is_localhost": isLocal,
		"role":         role,
		"authenticated": role == "admin" || role == "entry",
		"port":         port,
		"lan_urls":     urls,
		"allow_lan":    cfgCopy.AllowLan,
	}
	if isLocal {
		resp["admin_passcode"] = cfgCopy.AdminPasscode
		resp["entry_passcode"] = cfgCopy.EntryPasscode
	}
	writeJSON(w, http.StatusOK, resp)
}

func handleAuthLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
		return
	}
	var req struct {
		Passcode string `json:"passcode"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	pass := strings.TrimSpace(req.Passcode)

	cfgMutex.RLock()
	adminPass := appCfg.AdminPasscode
	entryPass := appCfg.EntryPasscode
	allowLan := appCfg.AllowLan
	cfgMutex.RUnlock()

	if !allowLan && !isLocalRequest(r) {
		writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "主机已关闭局域网访问"})
		return
	}

	if pass != "" && pass == adminPass {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "role": "admin", "token": signRoleToken("admin")})
		return
	}
	if pass != "" && pass == entryPass {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "role": "entry", "token": signRoleToken("entry")})
		return
	}
	writeJSON(w, http.StatusUnauthorized, map[string]interface{}{"success": false, "message": "口令错误！请输入内网登记口令或管理员口令"})
}

func handleUpdateSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
		return
	}
	if getRequestRole(r) != "admin" {
		writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "仅管理员可修改系统设置"})
		return
	}
	var req struct {
		Port          int    `json:"port"`
		AdminPasscode string `json:"admin_passcode"`
		EntryPasscode string `json:"entry_passcode"`
		AllowLan      bool   `json:"allow_lan"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "参数格式错误"})
		return
	}
	if req.Port < 1024 || req.Port > 65535 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "端口范围必须在 1024 ~ 65535 之间"})
		return
	}
	if strings.TrimSpace(req.AdminPasscode) == "" || strings.TrimSpace(req.EntryPasscode) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "管理员口令与内网登记口令均不能为空"})
		return
	}
	if strings.TrimSpace(req.AdminPasscode) == strings.TrimSpace(req.EntryPasscode) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "管理员口令与内网登记口令不能相同，以便区分权限"})
		return
	}

	cfgMutex.Lock()
	oldPort := appCfg.Port
	appCfg.Port = req.Port
	appCfg.AdminPasscode = strings.TrimSpace(req.AdminPasscode)
	appCfg.EntryPasscode = strings.TrimSpace(req.EntryPasscode)
	appCfg.AllowLan = req.AllowLan
	cfgMutex.Unlock()

	if err := saveConfigToFile(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	needRestart := oldPort != req.Port
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":      true,
		"need_restart": needRestart,
		"new_port":     req.Port,
		"message":      "系统配置已保存",
	})

	if needRestart {
		go func(newP int) {
			time.Sleep(400 * time.Millisecond)
			_ = restartHTTPServer(newP)
		}(req.Port)
	}
}

// ==================== 6. 用品字典管理（防重复 + 修改数量守恒折算） ====================

type ItemDTO struct {
	ID        int64   `json:"id"`
	Category  string  `json:"category"`
	Name      string  `json:"name"`
	Nature    string  `json:"nature"`
	BaseUnit  string  `json:"base_unit"`
	PackUnit  string  `json:"pack_unit"`
	PackSize  float64 `json:"pack_size"`
	MinPrice  float64 `json:"min_price"`
	MaxPrice  float64 `json:"max_price"`
	StockQty  float64 `json:"stock_qty"`
	AssetCnt  int     `json:"asset_cnt"`
	CreatedAt string  `json:"created_at"`
}

func recalculateItemPriceRange(itemID int64) {
	var minP, maxP sql.NullFloat64
	_ = db.QueryRow(`SELECT MIN(unit_price), MAX(unit_price) FROM consumable_ledger WHERE item_id = ? AND tx_type = 'IN' AND unit_price > 0`, itemID).Scan(&minP, &maxP)
	if !minP.Valid {
		_ = db.QueryRow(`SELECT MIN(unit_price), MAX(unit_price) FROM reusable_assets WHERE item_id = ? AND unit_price > 0`, itemID).Scan(&minP, &maxP)
	}
	_, _ = db.Exec(`UPDATE items SET min_price = ?, max_price = ?, updated_at = ? WHERE id = ?`, round2(minP.Float64), round2(maxP.Float64), nowStr(), itemID)
}

func handleItems(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		nature := r.URL.Query().Get("nature")
		category := r.URL.Query().Get("category")
		kw := strings.TrimSpace(r.URL.Query().Get("kw"))

		query := `SELECT id, category, name, nature, base_unit, pack_unit, pack_size, min_price, max_price, created_at FROM items WHERE 1=1`
		var args []interface{}
		if nature != "" {
			query += ` AND nature = ?`
			args = append(args, nature)
		}
		if category != "" {
			query += ` AND category = ?`
			args = append(args, category)
		}
		if kw != "" {
			query += ` AND (name LIKE ? OR category LIKE ?)`
			args = append(args, "%"+kw+"%", "%"+kw+"%")
		}
		query += ` ORDER BY category ASC, id ASC`

		rows, err := db.Query(query, args...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer rows.Close()

		list := make([]ItemDTO, 0)
		for rows.Next() {
			var it ItemDTO
			if err := rows.Scan(&it.ID, &it.Category, &it.Name, &it.Nature, &it.BaseUnit, &it.PackUnit, &it.PackSize, &it.MinPrice, &it.MaxPrice, &it.CreatedAt); err != nil {
				continue
			}
			if it.Nature == "consumable" {
				it.StockQty = calcConsumableStockAtDate(it.ID, "9999-12-31", true)
			} else {
				_ = db.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE item_id = ? AND status != '报废'`, it.ID).Scan(&it.AssetCnt)
			}
			list = append(list, it)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})

	case http.MethodPost:
		var req struct {
			Category string  `json:"category"`
			Name     string  `json:"name"`
			Nature   string  `json:"nature"`
			BaseUnit string  `json:"base_unit"`
			PackUnit string  `json:"pack_unit"`
			PackSize float64 `json:"pack_size"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请求参数错误"})
			return
		}
		req.Category = strings.TrimSpace(req.Category)
		req.Name = strings.TrimSpace(req.Name)
		req.BaseUnit = strings.TrimSpace(req.BaseUnit)
		req.PackUnit = strings.TrimSpace(req.PackUnit)
		if req.Category == "" || req.Name == "" || req.BaseUnit == "" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "用品分类、品名、基本单位为必填项"})
			return
		}
		if req.Nature != "consumable" && req.Nature != "reusable" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "性质必须为消耗品或复用品"})
			return
		}
		if req.PackSize <= 0 {
			req.PackSize = 1
		}

		var exists int
		_ = db.QueryRow(`SELECT COUNT(1) FROM items WHERE category = ? AND nature = ? AND name = ?`, req.Category, req.Nature, req.Name).Scan(&exists)
		if exists > 0 {
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("重复拦截：分类【%s】下的【%s】已存在同名用品【%s】！", req.Category, natureLabel(req.Nature), req.Name),
			})
			return
		}

		now := nowStr()
		res, err := db.Exec(`INSERT INTO items (category, name, nature, base_unit, pack_unit, pack_size, min_price, max_price, created_at, updated_at) VALUES (?,?,?,?,?,?,0,0,?,?)`,
			req.Category, req.Name, req.Nature, req.BaseUnit, req.PackUnit, req.PackSize, now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": id})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
	}
}

func handleItemByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/items/")
	itemID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "无效的用品ID"})
		return
	}

	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
		return
	}

	var req struct {
		Category      string  `json:"category"`
		Name          string  `json:"name"`
		BaseUnit      string  `json:"base_unit"`
		PackUnit      string  `json:"pack_unit"`
		PackSize      float64 `json:"pack_size"`
		AdminPasscode string  `json:"admin_passcode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "参数格式错误"})
		return
	}
	if !verifyAdminPasscode(req.AdminPasscode) {
		writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：管理员确认口令不正确！"})
		return
	}

	var oldCat, oldName, nature, oldBaseUnit, oldPackUnit string
	var oldPackSize float64
	err = db.QueryRow(`SELECT category, name, nature, base_unit, pack_unit, pack_size FROM items WHERE id = ?`, itemID).Scan(&oldCat, &oldName, &nature, &oldBaseUnit, &oldPackUnit, &oldPackSize)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "未找到该用品信息"})
		return
	}

	req.Category = strings.TrimSpace(req.Category)
	req.Name = strings.TrimSpace(req.Name)
	req.BaseUnit = strings.TrimSpace(req.BaseUnit)
	req.PackUnit = strings.TrimSpace(req.PackUnit)
	if req.PackSize <= 0 {
		req.PackSize = 1
	}

	// 检查修改后是否与其他同分类同性质用品重名
	var dup int
	_ = db.QueryRow(`SELECT COUNT(1) FROM items WHERE category = ? AND nature = ? AND name = ? AND id != ?`, req.Category, nature, req.Name, itemID).Scan(&dup)
	if dup > 0 {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "message": "修改失败：目标分类下已存在同名同性质用品！"})
		return
	}

	tx, err := db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(`UPDATE items SET category = ?, name = ?, base_unit = ?, pack_unit = ?, pack_size = ?, updated_at = ? WHERE id = ?`,
		req.Category, req.Name, req.BaseUnit, req.PackUnit, req.PackSize, nowStr(), itemID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	// 核心规则：已登记流水以【基本数量】为准，包装数量按新的 pack_size 自动重新折算！
	_, err = tx.Exec(`UPDATE consumable_ledger SET pack_qty = ROUND(base_qty / ?, 4), updated_at = ? WHERE item_id = ?`, req.PackSize, nowStr(), itemID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	writeAudit("修改基础用品字典", fmt.Sprintf("ID=%d (%s)", itemID, req.Name),
		fmt.Sprintf("原[%s/%s, 包装规格:%g] -> 新[%s/%s, 包装规格:%g]，已按基本数量重算历史包装数", oldCat, oldName, oldPackSize, req.Category, req.Name, req.PackSize))

	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "用品信息已修改，历史台账包装数量已按基本数量重新折算"})
}

func natureLabel(n string) string {
	if n == "reusable" {
		return "复用品(类资产)"
	}
	return "消耗品"
}

// ==================== 7. 消耗品入库与领用流水账（按年独立流水号） ====================

func generateNextConsumableSerial(tx *sql.Tx, year int) (int, string, error) {
	var maxSeq sql.NullInt64
	err := tx.QueryRow(`SELECT MAX(seq_num) FROM consumable_ledger WHERE ledger_year = ?`, year).Scan(&maxSeq)
	if err != nil {
		return 0, "", err
	}
	nextSeq := int(maxSeq.Int64) + 1
	serialNo := fmt.Sprintf("XH-%d-%06d", year, nextSeq)
	return nextSeq, serialNo, nil
}

func handleConsumableLedger(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		category := r.URL.Query().Get("category")
		itemID := r.URL.Query().Get("item_id")
		txType := r.URL.Query().Get("tx_type")
		startDate := r.URL.Query().Get("start_date")
		endDate := r.URL.Query().Get("end_date")
		yearStr := r.URL.Query().Get("year")

		q := `
		SELECT cl.id, cl.ledger_year, cl.seq_num, cl.serial_no, cl.item_id,
		       i.category, i.name, i.base_unit, i.pack_unit, i.pack_size,
		       cl.tx_type, cl.tx_date, cl.base_qty, cl.pack_qty, cl.total_amount, cl.unit_price,
		       cl.recipient, cl.remark, cl.created_at
		FROM consumable_ledger cl
		JOIN items i ON cl.item_id = i.id
		WHERE 1=1`
		var args []interface{}
		if category != "" {
			q += ` AND i.category = ?`
			args = append(args, category)
		}
		if itemID != "" {
			q += ` AND cl.item_id = ?`
			args = append(args, itemID)
		}
		if txType != "" {
			q += ` AND cl.tx_type = ?`
			args = append(args, txType)
		}
		if yearStr != "" {
			q += ` AND cl.ledger_year = ?`
			args = append(args, yearStr)
		}
		if startDate != "" {
			q += ` AND cl.tx_date >= ?`
			args = append(args, startDate)
		}
		if endDate != "" {
			q += ` AND cl.tx_date <= ?`
			args = append(args, endDate)
		}
		q += ` ORDER BY cl.tx_date ASC, cl.ledger_year ASC, cl.seq_num ASC, cl.id ASC`

		rows, err := db.Query(q, args...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer rows.Close()

		list := make([]map[string]interface{}, 0)
		for rows.Next() {
			var id, lYear, seqNum, itID int64
			var serialNo, cat, name, bUnit, pUnit, tType, tDate, recip, rmk, createdAt string
			var pSize, bQty, pQty, totAmt, uPrice float64
			if err := rows.Scan(&id, &lYear, &seqNum, &serialNo, &itID, &cat, &name, &bUnit, &pUnit, &pSize,
				&tType, &tDate, &bQty, &pQty, &totAmt, &uPrice, &recip, &rmk, &createdAt); err != nil {
				continue
			}
			list = append(list, map[string]interface{}{
				"id":           id,
				"ledger_year":  lYear,
				"seq_num":      seqNum,
				"serial_no":    serialNo,
				"item_id":      itID,
				"category":     cat,
				"item_name":    name,
				"base_unit":    bUnit,
				"pack_unit":    pUnit,
				"pack_size":    pSize,
				"tx_type":      tType,
				"tx_date":      tDate,
				"base_qty":     bQty,
				"pack_qty":     pQty,
				"total_amount": totAmt,
				"unit_price":   uPrice,
				"recipient":    recip,
				"remark":       rmk,
				"created_at":   createdAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})

	case http.MethodPost:
		var req struct {
			ItemID      int64   `json:"item_id"`
			TxType      string  `json:"tx_type"`
			TxDate      string  `json:"tx_date"`
			BaseQty     float64 `json:"base_qty"`
			TotalAmount float64 `json:"total_amount"`
			Recipient   string  `json:"recipient"`
			Remark      string  `json:"remark"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "参数格式错误"})
			return
		}
		if req.TxDate == "" {
			req.TxDate = todayStr()
		}
		if req.BaseQty <= 0 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "登记数量必须大于 0"})
			return
		}
		if req.TxType != "IN" && req.TxType != "OUT" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "操作类型必须为入库(IN)或领用(OUT)"})
			return
		}

		var nature string
		var packSize float64
		err := db.QueryRow(`SELECT nature, pack_size FROM items WHERE id = ?`, req.ItemID).Scan(&nature, &packSize)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "未找到所选用品"})
			return
		}
		if nature != "consumable" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "复用品请通过类资产通道登记，不可走消耗品流水"})
			return
		}

		if req.TxType == "OUT" {
			if strings.TrimSpace(req.Recipient) == "" {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "领用登记必须填写领用人"})
				return
			}
			curStock := calcConsumableStockAtDate(req.ItemID, "9999-12-31", true)
			if req.BaseQty > curStock+0.0001 {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"message": fmt.Sprintf("库存不足！当前可用结存仅为 %g，无法领用 %g", round4(curStock), req.BaseQty),
				})
				return
			}
		}

		yearPart, err := strconv.Atoi(req.TxDate[:4])
		if err != nil {
			yearPart = time.Now().Year()
		}

		packQty := round4(req.BaseQty / packSize)
		unitPrice := 0.0
		if req.TxType == "IN" && req.TotalAmount > 0 {
			unitPrice = round4(req.TotalAmount / req.BaseQty)
		}

		tx, err := db.Begin()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer tx.Rollback()

		seqNum, serialNo, err := generateNextConsumableSerial(tx, yearPart)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		now := nowStr()
		res, err := tx.Exec(`
			INSERT INTO consumable_ledger (ledger_year, seq_num, serial_no, item_id, tx_type, tx_date, base_qty, pack_qty, total_amount, unit_price, recipient, remark, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			yearPart, seqNum, serialNo, req.ItemID, req.TxType, req.TxDate, req.BaseQty, packQty, round2(req.TotalAmount), unitPrice, strings.TrimSpace(req.Recipient), strings.TrimSpace(req.Remark), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		newID, _ := res.LastInsertId()

		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		if req.TxType == "IN" {
			recalculateItemPriceRange(req.ItemID)
		} else {
			recordRecipient(req.Recipient)
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": newID, "serial_no": serialNo})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
	}
}

func handleConsumableLedgerByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/consumables/ledger/")
	ledgerID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "无效记录ID"})
		return
	}

	if r.Method != http.MethodPut {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
		return
	}

	var req struct {
		TxDate        string  `json:"tx_date"`
		BaseQty       float64 `json:"base_qty"`
		TotalAmount   float64 `json:"total_amount"`
		Recipient     string  `json:"recipient"`
		Remark        string  `json:"remark"`
		AdminPasscode string  `json:"admin_passcode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "参数错误"})
		return
	}
	if !verifyAdminPasscode(req.AdminPasscode) {
		writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：修改已登记流水需要输入正确的管理员口令！"})
		return
	}
	if req.BaseQty <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "基本数量必须大于 0"})
		return
	}

	var itemID int64
	var serialNo, txType, oldDate, oldRecip string
	var oldQty, oldAmt float64
	err = db.QueryRow(`SELECT item_id, serial_no, tx_type, tx_date, base_qty, total_amount, recipient FROM consumable_ledger WHERE id = ?`, ledgerID).
		Scan(&itemID, &serialNo, &txType, &oldDate, &oldQty, &oldAmt, &oldRecip)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "流水记录不存在"})
		return
	}

	var packSize float64
	_ = db.QueryRow(`SELECT pack_size FROM items WHERE id = ?`, itemID).Scan(&packSize)
	if packSize <= 0 {
		packSize = 1
	}

	// 库存防负数校验
	curTotalStock := calcConsumableStockAtDate(itemID, "9999-12-31", true)
	var simulatedStock float64
	if txType == "IN" {
		simulatedStock = curTotalStock - oldQty + req.BaseQty
	} else {
		simulatedStock = curTotalStock + oldQty - req.BaseQty
	}
	if simulatedStock < -0.0001 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "修改冲突：按此数量修改后会导致当前总库存变为负数！"})
		return
	}

	packQty := round4(req.BaseQty / packSize)
	unitPrice := 0.0
	if txType == "IN" && req.TotalAmount > 0 {
		unitPrice = round4(req.TotalAmount / req.BaseQty)
	}

	_, err = db.Exec(`UPDATE consumable_ledger SET tx_date = ?, base_qty = ?, pack_qty = ?, total_amount = ?, unit_price = ?, recipient = ?, remark = ?, updated_at = ? WHERE id = ?`,
		req.TxDate, req.BaseQty, packQty, round2(req.TotalAmount), unitPrice, strings.TrimSpace(req.Recipient), strings.TrimSpace(req.Remark), nowStr(), ledgerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	if txType == "IN" {
		recalculateItemPriceRange(itemID)
	} else if req.Recipient != "" {
		recordRecipient(req.Recipient)
	}

	writeAudit("修正消耗品流水记录", serialNo, fmt.Sprintf("原[日期:%s, 数量:%g, 金额:%g] -> 新[日期:%s, 数量:%g, 金额:%g]", oldDate, oldQty, oldAmt, req.TxDate, req.BaseQty, req.TotalAmount))
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "流水记录已修正并写入审计日志"})
}

// ==================== 8. 会计级期初/期末盘点统计与年度结转引擎 ====================

// 计算指定日期（含当日或不含当日）的消耗品库存结存
func calcConsumableStockAtDate(itemID int64, targetDate string, inclusive bool) float64 {
	// 1. 查找目标日期之前最近的一次年度结转点
	var carryYear int
	var carryQty float64
	err := db.QueryRow(`SELECT carry_year, closing_base_qty FROM annual_carryover WHERE item_id = ? AND (carry_year || '-12-31') < ? ORDER BY carry_year DESC LIMIT 1`,
		itemID, targetDate).Scan(&carryYear, &carryQty)

	startBoundary := "0000-01-01"
	baseStock := 0.0
	if err == nil {
		baseStock = carryQty
		startBoundary = fmt.Sprintf("%d-01-01", carryYear+1)
	}

	op := "<"
	if inclusive {
		op = "<="
	}

	var inSum, outSum float64
	q := fmt.Sprintf(`
		SELECT
			COALESCE(SUM(CASE WHEN tx_type = 'IN' THEN base_qty ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN tx_type = 'OUT' THEN base_qty ELSE 0 END), 0)
		FROM consumable_ledger
		WHERE item_id = ? AND tx_date >= ? AND tx_date %s ?`, op)
	_ = db.QueryRow(q, itemID, startBoundary, targetDate).Scan(&inSum, &outSum)

	return round4(baseStock + inSum - outSum)
}

func handleConsumableInventory(w http.ResponseWriter, r *http.Request) {
	startDate := r.URL.Query().Get("start_date")
	endDate := r.URL.Query().Get("end_date")
	category := r.URL.Query().Get("category")
	itemIDStr := r.URL.Query().Get("item_id")

	if startDate == "" {
		startDate = fmt.Sprintf("%d-01-01", time.Now().Year())
	}
	if endDate == "" {
		endDate = todayStr()
	}

	q := `SELECT id, category, name, base_unit, pack_unit, pack_size, min_price, max_price FROM items WHERE nature = 'consumable'`
	var args []interface{}
	if category != "" {
		q += ` AND category = ?`
		args = append(args, category)
	}
	if itemIDStr != "" {
		q += ` AND id = ?`
		args = append(args, itemIDStr)
	}
	q += ` ORDER BY category ASC, id ASC`

	rows, err := db.Query(q, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer rows.Close()

	itemsList := make([]map[string]interface{}, 0)
	catSummaryMap := make(map[string]map[string]interface{})

	for rows.Next() {
		var id int64
		var cat, name, bUnit, pUnit string
		var pSize, minP, maxP float64
		if err := rows.Scan(&id, &cat, &name, &bUnit, &pUnit, &pSize, &minP, &maxP); err != nil {
			continue
		}

		openingQty := calcConsumableStockAtDate(id, startDate, false)
		var periodInQty, periodOutQty, periodInAmt float64
		_ = db.QueryRow(`
			SELECT
				COALESCE(SUM(CASE WHEN tx_type = 'IN' THEN base_qty ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN tx_type = 'OUT' THEN base_qty ELSE 0 END), 0),
				COALESCE(SUM(CASE WHEN tx_type = 'IN' THEN total_amount ELSE 0 END), 0)
			FROM consumable_ledger
			WHERE item_id = ? AND tx_date >= ? AND tx_date <= ?`, id, startDate, endDate).Scan(&periodInQty, &periodOutQty, &periodInAmt)

		closingQty := round4(openingQty + periodInQty - periodOutQty)

		rowObj := map[string]interface{}{
			"item_id":          id,
			"category":         cat,
			"item_name":        name,
			"base_unit":        bUnit,
			"pack_unit":        pUnit,
			"pack_size":        pSize,
			"opening_base_qty": openingQty,
			"opening_pack_qty": round4(openingQty / pSize),
			"in_base_qty":      round4(periodInQty),
			"in_pack_qty":      round4(periodInQty / pSize),
			"out_base_qty":     round4(periodOutQty),
			"out_pack_qty":     round4(periodOutQty / pSize),
			"closing_base_qty": closingQty,
			"closing_pack_qty": round4(closingQty / pSize),
			"period_in_amount": round2(periodInAmt),
			"price_range":      formatPriceRange(minP, maxP),
		}
		itemsList = append(itemsList, rowObj)

		if _, ok := catSummaryMap[cat]; !ok {
			catSummaryMap[cat] = map[string]interface{}{
				"category":       cat,
				"item_count":     0,
				"total_in_amt":   0.0,
				"active_skus":    0,
			}
		}
		catSummaryMap[cat]["item_count"] = catSummaryMap[cat]["item_count"].(int) + 1
		catSummaryMap[cat]["total_in_amt"] = round2(catSummaryMap[cat]["total_in_amt"].(float64) + periodInAmt)
		if closingQty > 0 {
			catSummaryMap[cat]["active_skus"] = catSummaryMap[cat]["active_skus"].(int) + 1
		}
	}

	catList := make([]map[string]interface{}, 0, len(catSummaryMap))
	for _, v := range catSummaryMap {
		catList = append(catList, v)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":          true,
		"start_date":       startDate,
		"end_date":         endDate,
		"items":            itemsList,
		"category_summary": catList,
	})
}

func formatPriceRange(minP, maxP float64) string {
	if minP <= 0 && maxP <= 0 {
		return "暂无入库价"
	}
	if math.Abs(minP-maxP) < 0.001 {
		return fmt.Sprintf("¥%.2f", minP)
	}
	return fmt.Sprintf("¥%.2f ~ ¥%.2f", minP, maxP)
}

func handleAnnualCarryover(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		rows, err := db.Query(`SELECT carry_year, COUNT(1), MAX(created_at) FROM annual_carryover GROUP BY carry_year ORDER BY carry_year DESC`)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer rows.Close()
		list := make([]map[string]interface{}, 0)
		for rows.Next() {
			var y, cnt int
			var ts string
			_ = rows.Scan(&y, &cnt, &ts)
			list = append(list, map[string]interface{}{"carry_year": y, "item_count": cnt, "created_at": ts})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
		return
	}

	var req struct {
		CarryYear     int    `json:"carry_year"`
		AdminPasscode string `json:"admin_passcode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "参数错误"})
		return
	}
	if !verifyAdminPasscode(req.AdminPasscode) {
		writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：执行年度结转需要管理员口令！"})
		return
	}
	if req.CarryYear < 2000 || req.CarryYear > 2100 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请输入有效的结转年份（如 2026）"})
		return
	}

	// 结转前自动备份数据库
	autoDailyBackup()

	yearEndDate := fmt.Sprintf("%d-12-31", req.CarryYear)
	rows, err := db.Query(`SELECT id FROM items WHERE nature = 'consumable'`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	var itemIDs []int64
	for rows.Next() {
		var id int64
		_ = rows.Scan(&id)
		itemIDs = append(itemIDs, id)
	}
	rows.Close()

	now := nowStr()
	tx, err := db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer tx.Rollback()

	for _, itID := range itemIDs {
		closingQty := calcConsumableStockAtDate(itID, yearEndDate, true)
		_, err := tx.Exec(`
			INSERT INTO annual_carryover (carry_year, item_id, closing_base_qty, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT(carry_year, item_id) DO UPDATE SET closing_base_qty = excluded.closing_base_qty, created_at = excluded.created_at`,
			req.CarryYear, itID, closingQty, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	// 统计复用品当前在用与报废数量作为年度结转摘要
	var inUseCnt, idleCnt, scrapCnt int
	_ = db.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE status = '在用'`).Scan(&inUseCnt)
	_ = db.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE status = '闲置'`).Scan(&idleCnt)
	_ = db.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE status = '报废'`).Scan(&scrapCnt)

	writeAudit("执行年度结转", fmt.Sprintf("%d年度", req.CarryYear),
		fmt.Sprintf("消耗品共结转 %d 个品种至 %d-01-01 期初；复用品类资产快照：在用 %d 件、闲置 %d 件、已报废 %d 件", len(itemIDs), req.CarryYear+1, inUseCnt, idleCnt, scrapCnt))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success": true,
		"message": fmt.Sprintf("%d 年度结转完成！次年消耗品流水号将从 XH-%d-000001 重新起编。复用品类资产统计：在用 %d 件 / 闲置 %d 件 / 报废 %d 件。",
			req.CarryYear, req.CarryYear+1, inUseCnt, idleCnt, scrapCnt),
	})
}

// ==================== 9. 复用品（类资产全生命周期管理与留痕） ====================

func saveUploadedFile(fileHeader io.Reader, origFilename, subFolder, customPrefix string) (string, error) {
	ext := filepath.Ext(origFilename)
	stem := sanitizeToken(strings.TrimSuffix(filepath.Base(origFilename), ext))
	targetDir := filepath.Join(attachRootDir, subFolder)
	if err := os.MkdirAll(targetDir, 0755); err != nil {
		return "", err
	}
	finalName := fmt.Sprintf("%s_%s%s", sanitizeToken(customPrefix), stem, ext)
	absTarget := filepath.Join(targetDir, finalName)
	if _, err := os.Stat(absTarget); err == nil {
		finalName = fmt.Sprintf("%s_%d_%s%s", sanitizeToken(customPrefix), time.Now().Unix(), stem, ext)
		absTarget = filepath.Join(targetDir, finalName)
	}
	out, err := os.Create(absTarget)
	if err != nil {
		return "", err
	}
	defer out.Close()
	if _, err := io.Copy(out, fileHeader); err != nil {
		return "", err
	}
	rel, err := filepath.Rel(attachRootDir, absTarget)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

func generateNextAssetNo(tx *sql.Tx, year int) (string, error) {
	prefix := fmt.Sprintf("ZC-%d-", year)
	var count int
	err := tx.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE asset_no LIKE ?`, prefix+"%").Scan(&count)
	if err != nil {
		return "", err
	}
	for seq := count + 1; ; seq++ {
		candidate := fmt.Sprintf("%s%04d", prefix, seq)
		var exists int
		_ = tx.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE asset_no = ?`, candidate).Scan(&exists)
		if exists == 0 {
			return candidate, nil
		}
	}
}

func generateNextReusableLogNo(tx *sql.Tx) string {
	var maxID sql.NullInt64
	_ = tx.QueryRow(`SELECT MAX(id) FROM reusable_logs`).Scan(&maxID)
	return fmt.Sprintf("FY-%s-%05d", time.Now().Format("200601"), maxID.Int64+1)
}

func handleReusableAssets(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		category := r.URL.Query().Get("category")
		itemID := r.URL.Query().Get("item_id")
		status := r.URL.Query().Get("status")
		kw := strings.TrimSpace(r.URL.Query().Get("kw"))

		q := `
		SELECT ra.id, ra.asset_no, ra.item_id, i.category, i.name, i.base_unit,
		       ra.sub_name, ra.purchase_date, ra.unit_price, ra.expected_years,
		       ra.status, ra.current_user, ra.image_path, ra.scrap_date, ra.scrap_file_path, ra.remark, ra.created_at
		FROM reusable_assets ra
		JOIN items i ON ra.item_id = i.id
		WHERE 1=1`
		var args []interface{}
		if category != "" {
			q += ` AND i.category = ?`
			args = append(args, category)
		}
		if itemID != "" {
			q += ` AND ra.item_id = ?`
			args = append(args, itemID)
		}
		if status != "" {
			q += ` AND ra.status = ?`
			args = append(args, status)
		}
		if kw != "" {
			q += ` AND (ra.asset_no LIKE ? OR ra.sub_name LIKE ? OR ra.current_user LIKE ? OR i.name LIKE ?)`
			like := "%" + kw + "%"
			args = append(args, like, like, like, like)
		}
		q += ` ORDER BY ra.id DESC`

		rows, err := db.Query(q, args...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer rows.Close()

		list := make([]map[string]interface{}, 0)
		var cntIdle, cntInUse, cntScrap int
		var valActive, valScrap float64

		for rows.Next() {
			var id, itID int64
			var expYears int
			var uPrice float64
			var assetNo, cat, itName, bUnit, subName, pDate, st, curUser, imgPath, sDate, sFile, rmk, cAt string
			if err := rows.Scan(&id, &assetNo, &itID, &cat, &itName, &bUnit, &subName, &pDate, &uPrice, &expYears,
				&st, &curUser, &imgPath, &sDate, &sFile, &rmk, &cAt); err != nil {
				continue
			}
			switch st {
			case "在用":
				cntInUse++
				valActive += uPrice
			case "报废":
				cntScrap++
				valScrap += uPrice
			default:
				cntIdle++
				valActive += uPrice
			}
			list = append(list, map[string]interface{}{
				"id":              id,
				"asset_no":        assetNo,
				"item_id":         itID,
				"category":        cat,
				"item_name":       itName,
				"base_unit":       bUnit,
				"sub_name":        subName,
				"purchase_date":   pDate,
				"unit_price":      uPrice,
				"expected_years":  expYears,
				"status":          st,
				"current_user":    curUser,
				"image_path":      imgPath,
				"scrap_date":      sDate,
				"scrap_file_path": sFile,
				"remark":          rmk,
				"created_at":      cAt,
			})
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"data":    list,
			"summary": map[string]interface{}{
				"total_count":  len(list),
				"idle_count":   cntIdle,
				"in_use_count": cntInUse,
				"scrap_count":  cntScrap,
				"active_value": round2(valActive),
				"scrap_value":  round2(valScrap),
			},
		})

	case http.MethodPost:
		// 复用品入库登记（multipart/form-data，强制要求上传对应图片附件）
		if err := r.ParseMultipartForm(30 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "表单解析失败"})
			return
		}
		itemID, _ := strconv.ParseInt(r.FormValue("item_id"), 10, 64)
		qty, _ := strconv.Atoi(r.FormValue("quantity"))
		if qty <= 0 {
			qty = 1
		}
		totalAmt, _ := strconv.ParseFloat(r.FormValue("total_amount"), 64)
		expectedYears, _ := strconv.Atoi(r.FormValue("expected_years"))
		if expectedYears <= 0 {
			expectedYears = 10 // 默认 10 年
		}
		purchaseDate := strings.TrimSpace(r.FormValue("purchase_date"))
		if purchaseDate == "" {
			purchaseDate = todayStr()
		}
		subName := strings.TrimSpace(r.FormValue("sub_name"))
		remark := strings.TrimSpace(r.FormValue("remark"))

		var cat, itName, nature string
		err := db.QueryRow(`SELECT category, name, nature FROM items WHERE id = ?`, itemID).Scan(&cat, &itName, &nature)
		if err != nil || nature != "reusable" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请选择有效的复用品(类资产)品名"})
			return
		}

		file, header, err := r.FormFile("image_file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "复用品入库必须上传对应的实物图片附件！"})
			return
		}
		defer file.Close()

		yearPart, _ := strconv.Atoi(purchaseDate[:4])
		if yearPart == 0 {
			yearPart = time.Now().Year()
		}
		unitPrice := round4(totalAmt / float64(qty))
		now := nowStr()

		tx, err := db.Begin()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer tx.Rollback()

		var createdNos []string
		var savedRelImg string

		for i := 0; i < qty; i++ {
			assetNo, err := generateNextAssetNo(tx, yearPart)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
				return
			}
			if i == 0 {
				prefix := fmt.Sprintf("%s_%s_%s", assetNo, itName, subName)
				savedRelImg, err = saveUploadedFile(file, header.Filename, filepath.Join("2_复用品实物图片", sanitizeToken(cat)), prefix)
				if err != nil {
					writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": "保存图片失败: " + err.Error()})
					return
				}
			}

			itemSub := subName
			if qty > 1 && subName != "" {
				itemSub = fmt.Sprintf("%s #%d", subName, i+1)
			}

			res, err := tx.Exec(`
				INSERT INTO reusable_assets (asset_no, item_id, sub_name, purchase_date, unit_price, expected_years, status, current_user, image_path, remark, created_at, updated_at)
				VALUES (?,?,?,?,?,?,'闲置','',?,?,?,?)`,
				assetNo, itemID, itemSub, purchaseDate, unitPrice, expectedYears, savedRelImg, remark, now, now)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
				return
			}
			assetID, _ := res.LastInsertId()
			logNo := generateNextReusableLogNo(tx)
			_, _ = tx.Exec(`INSERT INTO reusable_logs (log_no, asset_id, action_type, action_date, operator_or_user, detail, attachment_path, created_at) VALUES (?,?,?,?,?,?,?,?)`,
				logNo, assetID, "入库建档", purchaseDate, "仓库管理员",
				fmt.Sprintf("新购入库建档，二级品名：%s，预计使用：%d年，单价：¥%.2f", itemSub, expectedYears, unitPrice), savedRelImg, now)

			createdNos = append(createdNos, assetNo)
		}

		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		recalculateItemPriceRange(itemID)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":   true,
			"asset_nos": createdNos,
			"message":   fmt.Sprintf("已成功生成 %d 条类资产独立台账（%s）", qty, strings.Join(createdNos, ", ")),
		})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
	}
}

// 复用品全生命周期流转：领用分配 / 变更使用人 / 报废登记（含报废凭证上传）/ 信息修正
func handleReusableAssetAction(w http.ResponseWriter, r *http.Request) {
	trimmed := strings.TrimPrefix(r.URL.Path, "/api/reusables/assets/")
	parts := strings.Split(trimmed, "/")
	assetID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "无效资产ID"})
		return
	}

	if len(parts) == 2 && parts[1] == "logs" && r.Method == http.MethodGet {
		rows, err := db.Query(`SELECT id, log_no, action_type, action_date, operator_or_user, detail, attachment_path, created_at FROM reusable_logs WHERE asset_id = ? ORDER BY id ASC`, assetID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer rows.Close()
		logs := make([]map[string]interface{}, 0)
		for rows.Next() {
			var id int64
			var lNo, aType, aDate, opUser, det, att, cAt string
			_ = rows.Scan(&id, &lNo, &aType, &aDate, &opUser, &det, &att, &cAt)
			logs = append(logs, map[string]interface{}{
				"id": id, "log_no": lNo, "action_type": aType, "action_date": aDate,
				"operator_or_user": opUser, "detail": det, "attachment_path": att, "created_at": cAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": logs})
		return
	}

	if len(parts) == 2 && parts[1] == "transition" && r.Method == http.MethodPost {
		_ = r.ParseMultipartForm(20 << 20)
		action := r.FormValue("action") // ASSIGN(领用), TRANSFER(变更使用人), SCRAP(报废)
		actionDate := strings.TrimSpace(r.FormValue("action_date"))
		if actionDate == "" {
			actionDate = todayStr()
		}
		user := strings.TrimSpace(r.FormValue("user"))
		remark := strings.TrimSpace(r.FormValue("remark"))

		var assetNo, curStatus, curUser, itName string
		err := db.QueryRow(`SELECT ra.asset_no, ra.status, ra.current_user, i.name FROM reusable_assets ra JOIN items i ON ra.item_id = i.id WHERE ra.id = ?`, assetID).
			Scan(&assetNo, &curStatus, &curUser, &itName)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "未找到该资产"})
			return
		}
		if curStatus == "报废" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "该资产已登记报废，不可再进行领用或变更操作！"})
			return
		}

		tx, err := db.Begin()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer tx.Rollback()

		logNo := generateNextReusableLogNo(tx)
		now := nowStr()

		switch action {
		case "ASSIGN":
			if curStatus == "在用" {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"success": false,
					"message": fmt.Sprintf("重复领用拦截：该资产当前正由【%s】使用中！如需更换人员请点击【变更使用人】。", curUser),
				})
				return
			}
			if user == "" {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请填写领用人"})
				return
			}
			_, _ = tx.Exec(`UPDATE reusable_assets SET status = '在用', current_user = ?, updated_at = ? WHERE id = ?`, user, now, assetID)
			_, _ = tx.Exec(`INSERT INTO reusable_logs (log_no, asset_id, action_type, action_date, operator_or_user, detail, created_at) VALUES (?,?,?,?,?,?,?)`,
				logNo, assetID, "首次领用", actionDate, user, fmt.Sprintf("分配给【%s】使用。%s", user, remark), now)
			recordRecipient(user)

		case "TRANSFER":
			if user == "" {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请填写新的使用人"})
				return
			}
			_, _ = tx.Exec(`UPDATE reusable_assets SET status = '在用', current_user = ?, updated_at = ? WHERE id = ?`, user, now, assetID)
			_, _ = tx.Exec(`INSERT INTO reusable_logs (log_no, asset_id, action_type, action_date, operator_or_user, detail, created_at) VALUES (?,?,?,?,?,?,?)`,
				logNo, assetID, "变更使用人", actionDate, user, fmt.Sprintf("使用人由【%s】变更为【%s】。%s", curUser, user, remark), now)
			recordRecipient(user)

		case "SCRAP":
			file, header, err := r.FormFile("scrap_file")
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "报废登记必须上传报废审批文件或现场照片资料！"})
				return
			}
			defer file.Close()

			yearDir := actionDate[:4]
			savedScrapPath, err := saveUploadedFile(file, header.Filename, filepath.Join("3_复用品报废凭证", yearDir), fmt.Sprintf("%s_%s_报废凭证", assetNo, itName))
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": "保存报废凭证失败: " + err.Error()})
				return
			}
			_, _ = tx.Exec(`UPDATE reusable_assets SET status = '报废', scrap_date = ?, scrap_file_path = ?, updated_at = ? WHERE id = ?`, actionDate, savedScrapPath, now, assetID)
			_, _ = tx.Exec(`INSERT INTO reusable_logs (log_no, asset_id, action_type, action_date, operator_or_user, detail, attachment_path, created_at) VALUES (?,?,?,?,?,?,?,?)`,
				logNo, assetID, "登记报废", actionDate, curUser, fmt.Sprintf("登记资产报废（原使用人：%s）。%s", curUser, remark), savedScrapPath, now)

		default:
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "未知的流转动作"})
			return
		}

		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "log_no": logNo})
		return
	}

	writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false})
}

func handleAllReusableLogs(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	itemID := r.URL.Query().Get("item_id")
	q := `
		SELECT rl.id, rl.log_no, ra.asset_no, i.category, i.name, ra.sub_name,
		       rl.action_type, rl.action_date, rl.operator_or_user, rl.detail, rl.attachment_path, rl.created_at
		FROM reusable_logs rl
		JOIN reusable_assets ra ON rl.asset_id = ra.id
		JOIN items i ON ra.item_id = i.id
		WHERE 1=1`
	var args []interface{}
	if category != "" {
		q += ` AND i.category = ?`
		args = append(args, category)
	}
	if itemID != "" {
		q += ` AND ra.item_id = ?`
		args = append(args, itemID)
	}
	q += ` ORDER BY rl.id ASC`

	rows, err := db.Query(q, args...)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer rows.Close()

	list := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id int64
		var logNo, assetNo, cat, itName, subName, aType, aDate, opUser, det, att, cAt string
		_ = rows.Scan(&id, &logNo, &assetNo, &cat, &itName, &subName, &aType, &aDate, &opUser, &det, &att, &cAt)
		list = append(list, map[string]interface{}{
			"id": id, "log_no": logNo, "asset_no": assetNo, "category": cat,
			"item_name": itName, "sub_name": subName, "action_type": aType,
			"action_date": aDate, "operator_or_user": opUser, "detail": det,
			"attachment_path": att, "created_at": cAt,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})
}

// ==================== 10. 月度原始手写登记附件管理 ====================

func handleMonthlyAttachments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ym := strings.TrimSpace(r.URL.Query().Get("year_month"))
		q := `SELECT id, year_month, title, file_path, original_name, remark, uploaded_at FROM monthly_attachments WHERE 1=1`
		var args []interface{}
		if ym != "" {
			q += ` AND year_month LIKE ?`
			args = append(args, ym+"%")
		}
		q += ` ORDER BY year_month DESC, id DESC`

		rows, err := db.Query(q, args...)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer rows.Close()

		list := make([]map[string]interface{}, 0)
		for rows.Next() {
			var id int64
			var yMonth, title, fPath, oName, rmk, uAt string
			_ = rows.Scan(&id, &yMonth, &title, &fPath, &oName, &rmk, &uAt)
			list = append(list, map[string]interface{}{
				"id": id, "year_month": yMonth, "title": title,
				"file_path": fPath, "original_name": oName, "remark": rmk, "uploaded_at": uAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})

	case http.MethodPost:
		if err := r.ParseMultipartForm(40 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "上传解析失败"})
			return
		}
		ym := strings.TrimSpace(r.FormValue("year_month"))
		if ym == "" {
			ym = time.Now().Format("2006-01")
		}
		title := strings.TrimSpace(r.FormValue("title"))
		if title == "" {
			title = ym + "_月度原始手写领用登记表"
		}
		remark := strings.TrimSpace(r.FormValue("remark"))

		file, header, err := r.FormFile("file")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请选择要上传的月度登记扫描件/照片"})
			return
		}
		defer file.Close()

		yearDir := ym[:4]
		relPath, err := saveUploadedFile(file, header.Filename, filepath.Join("1_消耗品月度手写登记表", yearDir), fmt.Sprintf("%s_%s", ym, title))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		res, err := db.Exec(`INSERT INTO monthly_attachments (year_month, title, file_path, original_name, remark, uploaded_at) VALUES (?,?,?,?,?,?)`,
			ym, title, relPath, header.Filename, remark, nowStr())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		id, _ := res.LastInsertId()
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "id": id, "file_path": relPath})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
	}
}

func handleMonthlyAttachmentByID(w http.ResponseWriter, r *http.Request) {
	idStr := strings.TrimPrefix(r.URL.Path, "/api/monthly-attachments/")
	attID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "无效附件ID"})
		return
	}

	switch r.Method {
	case http.MethodPut:
		var req struct {
			YearMonth     string `json:"year_month"`
			Title         string `json:"title"`
			Remark        string `json:"remark"`
			AdminPasscode string `json:"admin_passcode"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if !verifyAdminPasscode(req.AdminPasscode) {
			writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：修改月度附件信息需管理员口令！"})
			return
		}
		_, err := db.Exec(`UPDATE monthly_attachments SET year_month = ?, title = ?, remark = ? WHERE id = ?`,
			strings.TrimSpace(req.YearMonth), strings.TrimSpace(req.Title), strings.TrimSpace(req.Remark), attID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeAudit("修改月度附件信息", fmt.Sprintf("ID=%d", attID), fmt.Sprintf("月份:%s, 标题:%s", req.YearMonth, req.Title))
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})

	case http.MethodDelete:
		pass := r.URL.Query().Get("admin_passcode")
		if !verifyAdminPasscode(pass) {
			writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：删除月度原始凭证需输入管理员口令！"})
			return
		}
		var fPath, title string
		if err := db.QueryRow(`SELECT file_path, title FROM monthly_attachments WHERE id = ?`, attID).Scan(&fPath, &title); err == nil {
			absPath := filepath.Join(attachRootDir, filepath.FromSlash(fPath))
			_ = os.Remove(absPath)
		}
		_, _ = db.Exec(`DELETE FROM monthly_attachments WHERE id = ?`, attID)
		writeAudit("删除月度原始附件", title, fmt.Sprintf("ID=%d, 文件=%s", attID, fPath))
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true})

	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
	}
}

func handleRecipients(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT name FROM recipients ORDER BY last_used_at DESC LIMIT 10`)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": []string{}})
		return
	}
	defer rows.Close()
	list := make([]string, 0)
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		list = append(list, n)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})
}

func handleAuditLogs(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, action, target_info, detail, created_at FROM audit_logs ORDER BY id DESC LIMIT 100`)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": []interface{}{}})
		return
	}
	defer rows.Close()
	list := make([]map[string]interface{}, 0)
	for rows.Next() {
		var id int64
		var act, tgt, det, cAt string
		_ = rows.Scan(&id, &act, &tgt, &det, &cAt)
		list = append(list, map[string]interface{}{"id": id, "action": act, "target_info": tgt, "detail": det, "created_at": cAt})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})
}

// ==================== 11. Excel 专业盘点与流水账导出 ====================

func handleExportExcel(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode") // consumable_inventory | consumable_ledger | reusable_assets
	startDate := r.URL.Query().Get("start_date")
	endDate := r.URL.Query().Get("end_date")
	if startDate == "" {
		startDate = fmt.Sprintf("%d-01-01", time.Now().Year())
	}
	if endDate == "" {
		endDate = todayStr()
	}

	f := excelize.NewFile()
	defer f.Close()

	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 11, Family: "Microsoft YaHei"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#E2E8F0"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "94A3B8", Style: 1},
			{Type: "right", Color: "94A3B8", Style: 1},
			{Type: "top", Color: "94A3B8", Style: 1},
			{Type: "bottom", Color: "94A3B8", Style: 2},
		},
	})
	cellStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Family: "Microsoft YaHei"},
		Alignment: &excelize.Alignment{Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "CBD5E1", Style: 1},
			{Type: "right", Color: "CBD5E1", Style: 1},
			{Type: "top", Color: "CBD5E1", Style: 1},
			{Type: "bottom", Color: "CBD5E1", Style: 1},
		},
	})

	fileName := "办公用品台账导出.xlsx"

	switch mode {
	case "consumable_ledger":
		sheet := "消耗品接续流水账"
		_ = f.SetSheetName("Sheet1", sheet)
		headers := []string{"流水编号", "登记日期", "用品分类", "品名", "操作类型", "基本数量", "基本单位", "折算包装数", "包装单位", "采购总价(元)", "折算单价(元)", "领用人", "备注"}
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`
			SELECT cl.serial_no, cl.tx_date, i.category, i.name, cl.tx_type, cl.base_qty, i.base_unit, cl.pack_qty, i.pack_unit, cl.total_amount, cl.unit_price, cl.recipient, cl.remark
			FROM consumable_ledger cl JOIN items i ON cl.item_id = i.id
			WHERE cl.tx_date >= ? AND cl.tx_date <= ?
			ORDER BY cl.tx_date ASC, cl.ledger_year ASC, cl.seq_num ASC`, startDate, endDate)
		rIdx := 2
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var sNo, tDate, cat, name, tType, bUnit, pUnit, recip, rmk string
				var bQty, pQty, tAmt, uPrice float64
				_ = rows.Scan(&sNo, &tDate, &cat, &name, &tType, &bQty, &bUnit, &pQty, &pUnit, &tAmt, &uPrice, &recip, &rmk)
				typeZh := "入库"
				if tType == "OUT" {
					typeZh = "领用"
				}
				vals := []interface{}{sNo, tDate, cat, name, typeZh, bQty, bUnit, pQty, pUnit, tAmt, uPrice, recip, rmk}
				for cIdx, v := range vals {
					cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
					_ = f.SetCellValue(sheet, cell, v)
					_ = f.SetCellStyle(sheet, cell, cell, cellStyle)
				}
				rIdx++
			}
		}
		fileName = fmt.Sprintf("消耗品接续流水账_%s至%s.xlsx", startDate, endDate)

	case "reusable_assets":
		sheet := "复用品类资产台账"
		_ = f.SetSheetName("Sheet1", sheet)
		headers := []string{"资产编号", "用品分类", "标准品名", "二级品名/位置", "入库日期", "购置单价(元)", "预计使用(年)", "当前状态", "当前使用人", "报废日期", "备注"}
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`
			SELECT ra.asset_no, i.category, i.name, ra.sub_name, ra.purchase_date, ra.unit_price, ra.expected_years, ra.status, ra.current_user, ra.scrap_date, ra.remark
			FROM reusable_assets ra JOIN items i ON ra.item_id = i.id ORDER BY ra.id ASC`)
		rIdx := 2
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var aNo, cat, name, sub, pDate, st, user, sDate, rmk string
				var uPrice float64
				var expY int
				_ = rows.Scan(&aNo, &cat, &name, &sub, &pDate, &uPrice, &expY, &st, &user, &sDate, &rmk)
				vals := []interface{}{aNo, cat, name, sub, pDate, uPrice, expY, st, user, sDate, rmk}
				for cIdx, v := range vals {
					cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
					_ = f.SetCellValue(sheet, cell, v)
					_ = f.SetCellStyle(sheet, cell, cell, cellStyle)
				}
				rIdx++
			}
		}
		fileName = fmt.Sprintf("复用品类资产盘点台账_%s.xlsx", todayStr())

	default:
		sheet := "消耗品盘点统计表"
		_ = f.SetSheetName("Sheet1", sheet)
		headers := []string{"用品分类", "品名", "基本单位", "包装规格", "期初基本数", "期初包装数", "本期入库(基本)", "本期领用(基本)", "期末结存(基本)", "期末结存(包装)", "本期采购总额(元)", "历史采购单价区间"}
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 1)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`SELECT id, category, name, base_unit, pack_unit, pack_size, min_price, max_price FROM items WHERE nature = 'consumable' ORDER BY category ASC, id ASC`)
		rIdx := 2
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var id int64
				var cat, name, bUnit, pUnit string
				var pSize, minP, maxP float64
				_ = rows.Scan(&id, &cat, &name, &bUnit, &pUnit, &pSize, &minP, &maxP)
				openQty := calcConsumableStockAtDate(id, startDate, false)
				var inQty, outQty, inAmt float64
				_ = db.QueryRow(`SELECT COALESCE(SUM(CASE WHEN tx_type='IN' THEN base_qty ELSE 0 END),0), COALESCE(SUM(CASE WHEN tx_type='OUT' THEN base_qty ELSE 0 END),0), COALESCE(SUM(CASE WHEN tx_type='IN' THEN total_amount ELSE 0 END),0) FROM consumable_ledger WHERE item_id=? AND tx_date>=? AND tx_date<=?`,
					id, startDate, endDate).Scan(&inQty, &outQty, &inAmt)
				closeQty := round4(openQty + inQty - outQty)
				spec := fmt.Sprintf("1%s = %g%s", pUnit, pSize, bUnit)
				vals := []interface{}{cat, name, bUnit, spec, openQty, round4(openQty / pSize), inQty, outQty, closeQty, round4(closeQty / pSize), inAmt, formatPriceRange(minP, maxP)}
				for cIdx, v := range vals {
					cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
					_ = f.SetCellValue(sheet, cell, v)
					_ = f.SetCellStyle(sheet, cell, cell, cellStyle)
				}
				rIdx++
			}
		}
		fileName = fmt.Sprintf("消耗品盘点统计表_%s至%s.xlsx", startDate, endDate)
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	_ = f.Write(w)
}

// ==================== 12. 路由装配与 HTTP 服务热重启 ====================

func buildHTTPMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/status", handleAuthStatus)
	mux.HandleFunc("/api/auth/login", handleAuthLogin)
	mux.HandleFunc("/api/settings", handleUpdateSettings)

	mux.HandleFunc("/api/items", handleItems)
	mux.HandleFunc("/api/items/", handleItemByID)

	mux.HandleFunc("/api/consumables/ledger", handleConsumableLedger)
	mux.HandleFunc("/api/consumables/ledger/", handleConsumableLedgerByID)
	mux.HandleFunc("/api/consumables/inventory", handleConsumableInventory)
	mux.HandleFunc("/api/carryover", handleAnnualCarryover)

	mux.HandleFunc("/api/reusables/assets", handleReusableAssets)
	mux.HandleFunc("/api/reusables/assets/", handleReusableAssetAction)
	mux.HandleFunc("/api/reusables/logs", handleAllReusableLogs)

	mux.HandleFunc("/api/monthly-attachments", handleMonthlyAttachments)
	mux.HandleFunc("/api/monthly-attachments/", handleMonthlyAttachmentByID)

	mux.HandleFunc("/api/recipients", handleRecipients)
	mux.HandleFunc("/api/audit-logs", handleAuditLogs)
	mux.HandleFunc("/api/export/excel", handleExportExcel)

	// 静态附件安全访问（防目录穿越）
	mux.HandleFunc("/attachments/", func(w http.ResponseWriter, r *http.Request) {
		rel := strings.TrimPrefix(r.URL.Path, "/attachments/")
		cleanRel := filepath.Clean(filepath.FromSlash(rel))
		if strings.HasPrefix(cleanRel, "..") || filepath.IsAbs(cleanRel) {
			http.Error(w, "非法文件路径", http.StatusForbidden)
			return
		}
		absPath := filepath.Join(attachRootDir, cleanRel)
		if r.URL.Query().Get("download") == "1" {
			w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filepath.Base(absPath)))
		}
		http.ServeFile(w, r, absPath)
	})

	// 内嵌静态网页
	subWeb, _ := fs.Sub(embeddedWebFS, "web")
	mux.Handle("/", http.FileServer(http.FS(subWeb)))

	return authMiddleware(mux)
}

func startHTTPServer(port int) error {
	srvMutex.Lock()
	defer srvMutex.Unlock()

	ln, err := net.Listen("tcp", fmt.Sprintf("0.0.0.0:%d", port))
	if err != nil {
		return err
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port
	currentPort = actualPort

	srv := &http.Server{
		Handler: buildHTTPMux(),
	}
	httpSrv = srv

	go func() {
		log.Printf("✅ 办公用品与类资产台账系统已启动: http://127.0.0.1:%d", actualPort)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("HTTP 服务异常: %v", err)
		}
	}()
	return nil
}

func restartHTTPServer(newPort int) error {
	srvMutex.Lock()
	oldSrv := httpSrv
	srvMutex.Unlock()

	if oldSrv != nil {
		_ = oldSrv.Close()
	}
	return startHTTPServer(newPort)
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// ==================== 13. 系统托盘入口 ====================

func main() {
	if err := initDirectoriesAndConfig(); err != nil {
		log.Fatalf("初始化目录与配置失败: %v", err)
	}
	if err := initDatabase(); err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	if err := startHTTPServer(appCfg.Port); err != nil {
		log.Fatalf("启动端口 %d 失败（可能被占用）: %v", appCfg.Port, err)
	}

	if os.Getenv("HEADLESS_MODE") == "1" {
		select {}
	}

	systray.Run(onSystrayReady, onSystrayExit)
}

func onSystrayReady() {
	systray.SetTitle("办公用品台账")
	systray.SetTooltip(fmt.Sprintf("企业办公用品与类资产台账系统 (端口: %d)", currentPort))

	mOpen := systray.AddMenuItem("🌐 打开台账管理主页 (本机管理员)", "在浏览器中打开系统")
	mFolder := systray.AddMenuItem("📁 打开附件与数据文件夹", "查看归档的月度手写表与资产图片")
	systray.AddSeparator()
	mQuit := systray.AddMenuItem("❌ 退出系统", "安全关闭服务与数据库")

	openBrowser(fmt.Sprintf("http://127.0.0.1:%d", currentPort))

	go func() {
		for {
			select {
			case <-mOpen.ClickedCh:
				openBrowser(fmt.Sprintf("http://127.0.0.1:%d", currentPort))
			case <-mFolder.ClickedCh:
				openBrowser(dataRootDir)
			case <-mQuit.ClickedCh:
				systray.Quit()
				return
			}
		}
	}()
}

func onSystrayExit() {
	if httpSrv != nil {
		_ = httpSrv.Close()
	}
	if db != nil {
		_ = db.Close()
	}
}
