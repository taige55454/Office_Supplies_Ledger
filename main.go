package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/binary"
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
	"sort"
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
	OrgName       string `json:"org_name"`
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
			OrgName:       "综合行政部",
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
	if strings.TrimSpace(appCfg.OrgName) == "" {
		appCfg.OrgName = "综合行政部"
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
	tmpPath := configPath + ".tmp"
	if err := os.WriteFile(tmpPath, b, 0600); err != nil {
		return err
	}
	return os.Rename(tmpPath, configPath)
}

// ==================== 2. 数据库初始化与预置字典 ====================

func initDatabase() error {
	var err error
	db, err = sql.Open("sqlite", dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	schema := `
	CREATE TABLE IF NOT EXISTS items (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		category TEXT NOT NULL,
		name TEXT NOT NULL,
		nature TEXT NOT NULL CHECK(nature IN ('consumable', 'reusable')),
		base_unit TEXT NOT NULL,
		pack_unit TEXT NOT NULL DEFAULT '',
		pack_size INTEGER NOT NULL DEFAULT 1,
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
		base_qty INTEGER NOT NULL,
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
		closing_base_qty INTEGER NOT NULL,
		closing_value REAL NOT NULL DEFAULT 0,
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

	_, _ = db.Exec(`ALTER TABLE annual_carryover ADD COLUMN closing_value REAL NOT NULL DEFAULT 0`)

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

// ==================== 3. 通用工具函数、全年度连续无断号重排引擎、动态包装倒挤与移动加权成本引擎 ====================

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

func calcPackDisplay(baseQty int64, packSize int64) float64 {
	if packSize <= 0 {
		packSize = 1
	}
	if baseQty == 0 {
		return 0
	}
	return round2(float64(baseQty) / float64(packSize))
}

func calcDisposalDate(purchaseDate string, expectedYears int) string {
	if len(purchaseDate) < 10 {
		return "-"
	}
	t, err := time.Parse("2006-01-02", purchaseDate[:10])
	if err != nil {
		return "-"
	}
	if expectedYears <= 0 {
		expectedYears = 10
	}
	return t.AddDate(expectedYears, 0, 0).Format("2006-01-02")
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
	_, _ = db.Exec(`DELETE FROM recipients WHERE id NOT IN (SELECT id FROM recipients ORDER BY last_used_at DESC LIMIT 10)`)
}

// 解析流水编号 XH-2026-000003 -> (2026, 3, true)
func parseConsumableSerialNo(serialNo string) (int, int, bool) {
	s := strings.TrimSpace(strings.ToUpper(serialNo))
	parts := strings.Split(s, "-")
	if len(parts) != 3 || parts[0] != "XH" {
		return 0, 0, false
	}
	y, err1 := strconv.Atoi(parts[1])
	seq, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || y < 2000 || y > 2100 || seq <= 0 {
		return 0, 0, false
	}
	return y, seq, true
}

// 全年度连续无断号重排引擎：
// 将 focusID 插入到 year 年度的 desiredSeq 位置，自动闭合移动留下的空位并顺延后续序号，保证 1..N 绝对连续无断号！
func resequenceConsumableYear(tx *sql.Tx, year int, focusID int64, desiredSeq int) (string, error) {
	rows, err := tx.Query(`
		SELECT id, seq_num, serial_no
		FROM consumable_ledger
		WHERE ledger_year = ?
		ORDER BY seq_num ASC, id ASC`, year)
	if err != nil {
		return "", err
	}
	type seqNode struct {
		id    int64
		oldNo string
	}
	var others []seqNode
	var focusNode *seqNode

	for rows.Next() {
		var id int64
		var sNum int
		var sNo string
		if err := rows.Scan(&id, &sNum, &sNo); err == nil {
			if id == focusID {
				focusNode = &seqNode{id: id, oldNo: sNo}
			} else {
				others = append(others, seqNode{id: id, oldNo: sNo})
			}
		}
	}
	rows.Close()

	ordered := make([]seqNode, 0, len(others)+1)
	if focusNode != nil {
		insertIdx := desiredSeq - 1
		if insertIdx < 0 {
			insertIdx = 0
		}
		if insertIdx > len(others) {
			insertIdx = len(others)
		}
		ordered = append(ordered, others[:insertIdx]...)
		ordered = append(ordered, *focusNode)
		ordered = append(ordered, others[insertIdx:]...)
	} else {
		ordered = others
	}

	// 第一阶段：全部设为临时编号，彻底避免 UNIQUE 约束冲突
	for idx, node := range ordered {
		tmpNo := fmt.Sprintf("RESEQ-TMP-%d-%d", year, idx+1)
		if _, err := tx.Exec(`UPDATE consumable_ledger SET serial_no = ? WHERE id = ?`, tmpNo, node.id); err != nil {
			return "", err
		}
	}

	// 第二阶段：落位为 1..N 连续编号，并同步重命名受影响的物理附件
	finalFocusSerial := ""
	type renamePair struct{ oldPrefix, newPrefix string }
	var renames []renamePair

	for idx, node := range ordered {
		newSeq := idx + 1
		newNo := fmt.Sprintf("XH-%d-%06d", year, newSeq)
		if _, err := tx.Exec(`UPDATE consumable_ledger SET seq_num = ?, serial_no = ? WHERE id = ?`, newSeq, newNo, node.id); err != nil {
			return "", err
		}
		if node.id == focusID {
			finalFocusSerial = newNo
		}
		if node.oldNo != "" && !strings.HasPrefix(node.oldNo, "INIT-TMP-") && node.oldNo != newNo {
			renames = append(renames, renamePair{oldPrefix: node.oldNo, newPrefix: newNo})
		}
	}

	for _, rp := range renames {
		renameAttachmentsMatchingPrefix(rp.oldPrefix, rp.newPrefix)
	}
	return finalFocusSerial, nil
}

// 同步重命名附件目录中以旧编号开头的文件（先转临时名避免链式覆盖冲突）
func renameAttachmentsMatchingPrefix(oldPrefix, newPrefix string) {
	if oldPrefix == "" || newPrefix == "" || oldPrefix == newPrefix {
		return
	}
	_ = filepath.Walk(attachRootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		base := info.Name()
		if strings.HasPrefix(base, oldPrefix+"_") || strings.HasPrefix(base, oldPrefix+".") {
			newBase := newPrefix + strings.TrimPrefix(base, oldPrefix)
			newPath := filepath.Join(filepath.Dir(path), newBase)
			_ = os.Rename(path, newPath)
		}
		return nil
	})
}

// 每次登记、修正或修改包装规格时，实时对该品种全量流水（按流水编号顺序）执行【累计包装进度差额倒挤】并持久化
func reconcileItemPackQuantities(itemID int64) {
	var packSize int64
	if err := db.QueryRow(`SELECT pack_size FROM items WHERE id = ?`, itemID).Scan(&packSize); err != nil {
		return
	}
	if packSize <= 0 {
		packSize = 1
	}

	rows, err := db.Query(`
		SELECT id, tx_type, base_qty, pack_qty
		FROM consumable_ledger
		WHERE item_id = ?
		ORDER BY ledger_year ASC, seq_num ASC, id ASC`, itemID)
	if err != nil {
		return
	}

	type ledgerRec struct {
		id      int64
		txType  string
		baseQty int64
		oldPack float64
		newPack float64
	}
	var records []ledgerRec
	var cumInBase, cumOutBase int64
	var cumInPack, cumOutPack float64

	for rows.Next() {
		var r ledgerRec
		if err := rows.Scan(&r.id, &r.txType, &r.baseQty, &r.oldPack); err != nil {
			continue
		}
		if r.txType == "IN" {
			cumInBase += r.baseQty
			targetPack := round2(float64(cumInBase) / float64(packSize))
			r.newPack = round2(targetPack - cumInPack)
			cumInPack = targetPack
		} else {
			cumOutBase += r.baseQty
			targetPack := round2(float64(cumOutBase) / float64(packSize))
			r.newPack = round2(targetPack - cumOutPack)
			cumOutPack = targetPack
		}
		records = append(records, r)
	}
	rows.Close()

	for _, r := range records {
		if math.Abs(r.oldPack-r.newPack) > 0.0001 {
			_, _ = db.Exec(`UPDATE consumable_ledger SET pack_qty = ? WHERE id = ?`, r.newPack, r.id)
		}
	}
}

// 移动加权平均成本全周期重算引擎（按流水编号顺序推进）：精确计算每笔流水前后库存、出库成本与出库单价
type movingAvgSnapshot struct {
	StockQty        int64
	StockValue      float64
	MovingUnitPrice float64
	PeriodInQty     int64
	PeriodInValue   float64
	PeriodOutQty    int64
	PeriodOutValue  float64
	OpeningQty      int64
	OpeningValue    float64
}

type txReplayDetail struct {
	BeforeQty    int64
	AfterQty     int64
	OutAmount    float64
	OutUnitPrice float64
	HasNegative  bool
}

func replayConsumableMovingAvg(itemID int64, startDate, endDate string) (movingAvgSnapshot, map[int64]txReplayDetail) {
	var snap movingAvgSnapshot
	txMap := make(map[int64]txReplayDetail)

	rows, err := db.Query(`
		SELECT id, tx_type, tx_date, base_qty, total_amount
		FROM consumable_ledger
		WHERE item_id = ?
		ORDER BY ledger_year ASC, seq_num ASC, id ASC`, itemID)
	if err != nil {
		return snap, txMap
	}
	defer rows.Close()

	var curQty int64
	var curVal float64
	openingCaptured := false

	for rows.Next() {
		var id, qty int64
		var tType, tDate string
		var amt float64
		if err := rows.Scan(&id, &tType, &tDate, &qty, &amt); err != nil {
			continue
		}

		if !openingCaptured && startDate != "" && tDate >= startDate {
			snap.OpeningQty = curQty
			snap.OpeningValue = round2(curVal)
			openingCaptured = true
		}

		inPeriod := (startDate == "" || tDate >= startDate) && (endDate == "" || tDate <= endDate)
		beforeQty := curQty

		if tType == "IN" {
			curQty += qty
			curVal = round2(curVal + amt)
			txMap[id] = txReplayDetail{
				BeforeQty: beforeQty,
				AfterQty:  curQty,
			}
			if inPeriod {
				snap.PeriodInQty += qty
				snap.PeriodInValue = round2(snap.PeriodInValue + amt)
			}
		} else if tType == "OUT" {
			var outCost, outUnit float64
			hasNeg := false
			if qty > curQty {
				hasNeg = true
			}
			if curQty <= 0 {
				outCost = 0
				outUnit = 0
				curQty -= qty
			} else if qty >= curQty {
				outCost = round2(curVal)
				outUnit = round4(curVal / float64(curQty))
				curQty -= qty
				curVal = 0
			} else {
				avgPrice := curVal / float64(curQty)
				outUnit = round4(avgPrice)
				outCost = round2(avgPrice * float64(qty))
				curQty -= qty
				curVal = round2(curVal - outCost)
				if curVal < 0 {
					curVal = 0
				}
			}
			txMap[id] = txReplayDetail{
				BeforeQty:    beforeQty,
				AfterQty:     curQty,
				OutAmount:    outCost,
				OutUnitPrice: outUnit,
				HasNegative:  hasNeg,
			}
			if inPeriod {
				snap.PeriodOutQty += qty
				snap.PeriodOutValue = round2(snap.PeriodOutValue + outCost)
			}
		}
	}

	if !openingCaptured {
		snap.OpeningQty = curQty
		snap.OpeningValue = round2(curVal)
	}
	snap.StockQty = curQty
	snap.StockValue = round2(curVal)
	if curQty > 0 {
		snap.MovingUnitPrice = round4(curVal / float64(curQty))
	}
	return snap, txMap
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
	tk := r.Header.Get("X-Auth-Token")
	if tk == "" {
		tk = r.URL.Query().Get("token")
	}
	if tk != "" {
		if role, ok := verifyRoleToken(tk); ok {
			return role
		}
		return "guest"
	}
	if isLocalRequest(r) {
		return "admin"
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
		"success":       true,
		"is_localhost":  isLocal,
		"role":          role,
		"authenticated": role == "admin" || role == "entry",
		"port":          port,
		"lan_urls":      urls,
		"allow_lan":     cfgCopy.AllowLan,
		"org_name":      cfgCopy.OrgName,
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Port          int    `json:"port"`
		AdminPasscode string `json:"admin_passcode"`
		EntryPasscode string `json:"entry_passcode"`
		AllowLan      bool   `json:"allow_lan"`
		OrgName       string `json:"org_name"`
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
	if strings.TrimSpace(req.OrgName) != "" {
		appCfg.OrgName = strings.TrimSpace(req.OrgName)
	}
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

// ==================== 6. 用品字典管理（防重复 + 修改数量守恒与实时倒挤重算） ====================

type ItemDTO struct {
	ID         int64   `json:"id"`
	Category   string  `json:"category"`
	Name       string  `json:"name"`
	Nature     string  `json:"nature"`
	BaseUnit   string  `json:"base_unit"`
	PackUnit   string  `json:"pack_unit"`
	PackSize   int64   `json:"pack_size"`
	MinPrice   float64 `json:"min_price"`
	MaxPrice   float64 `json:"max_price"`
	StockQty   int64   `json:"stock_qty"`
	StockValue float64 `json:"stock_value"`
	AvgPrice   float64 `json:"avg_price"`
	AssetCnt   int     `json:"asset_cnt"`
	CreatedAt  string  `json:"created_at"`
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

		list := make([]ItemDTO, 0)
		for rows.Next() {
			var it ItemDTO
			if err := rows.Scan(&it.ID, &it.Category, &it.Name, &it.Nature, &it.BaseUnit, &it.PackUnit, &it.PackSize, &it.MinPrice, &it.MaxPrice, &it.CreatedAt); err != nil {
				continue
			}
			list = append(list, it)
		}
		rows.Close()

		for idx := range list {
			if list[idx].Nature == "consumable" {
				snap, _ := replayConsumableMovingAvg(list[idx].ID, "", "")
				list[idx].StockQty = snap.StockQty
				list[idx].StockValue = snap.StockValue
				list[idx].AvgPrice = snap.MovingUnitPrice
			} else {
				_ = db.QueryRow(`SELECT COUNT(1) FROM reusable_assets WHERE item_id = ? AND status != '报废'`, list[idx].ID).Scan(&list[idx].AssetCnt)
			}
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			Category string `json:"category"`
			Name     string `json:"name"`
			Nature   string `json:"nature"`
			BaseUnit string `json:"base_unit"`
			PackUnit string `json:"pack_unit"`
			PackSize int64  `json:"pack_size"`
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

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		Category      string `json:"category"`
		Name          string `json:"name"`
		BaseUnit      string `json:"base_unit"`
		PackUnit      string `json:"pack_unit"`
		PackSize      int64  `json:"pack_size"`
		AdminPasscode string `json:"admin_passcode"`
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
	var oldPackSize int64
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

	var dup int
	_ = db.QueryRow(`SELECT COUNT(1) FROM items WHERE category = ? AND nature = ? AND name = ? AND id != ?`, req.Category, nature, req.Name, itemID).Scan(&dup)
	if dup > 0 {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"success": false, "message": "修改失败：目标分类下已存在同名同性质用品！"})
		return
	}

	_, err = db.Exec(`UPDATE items SET category = ?, name = ?, base_unit = ?, pack_unit = ?, pack_size = ?, updated_at = ? WHERE id = ?`,
		req.Category, req.Name, req.BaseUnit, req.PackUnit, req.PackSize, nowStr(), itemID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	reconcileItemPackQuantities(itemID)

	writeAudit("修改基础用品字典", fmt.Sprintf("ID=%d (%s)", itemID, req.Name),
		fmt.Sprintf("原[%s/%s, 包装规格:%d] -> 新[%s/%s, 包装规格:%d]，已按基本数量重算并倒挤历史包装数", oldCat, oldName, oldPackSize, req.Category, req.Name, req.PackSize))

	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "message": "用品信息已修改，历史台账包装数量已按基本数量重新倒挤折算"})
}

func natureLabel(n string) string {
	if n == "reusable" {
		return "复用品(类资产)"
	}
	return "消耗品"
}

// ==================== 7. 消耗品入库与领用流水账（连续重排 + 日期倒挂标晚日期条目 + 结存前后数量） ====================

func handleConsumableLedger(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		category := r.URL.Query().Get("category")
		itemID := r.URL.Query().Get("item_id")
		txType := r.URL.Query().Get("tx_type")
		startDate := r.URL.Query().Get("start_date")
		endDate := r.URL.Query().Get("end_date")
		yearStr := r.URL.Query().Get("year")

		// 先按全量流水编号顺序查出，确保日期倒挂检测与移动加权结存前后数量基于全局真实序列计算
		q := `
		SELECT cl.id, cl.ledger_year, cl.seq_num, cl.serial_no, cl.item_id,
		       i.category, i.name, i.base_unit, i.pack_unit, i.pack_size,
		       cl.tx_type, cl.tx_date, cl.base_qty, cl.pack_qty, cl.total_amount, cl.unit_price,
		       cl.recipient, cl.remark, cl.created_at
		FROM consumable_ledger cl
		JOIN items i ON cl.item_id = i.id
		ORDER BY cl.ledger_year ASC, cl.seq_num ASC, cl.id ASC`

		rows, err := db.Query(q)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		type rawLedgerRow struct {
			id, lYear, seqNum, itID, pSize, bQty                int64
			serialNo, cat, name, bUnit, pUnit, tType, tDate     string
			recip, rmk, createdAt                               string
			pQty, totAmt, uPrice                                float64
		}
		var rawList []rawLedgerRow
		distinctItems := make(map[int64]bool)

		for rows.Next() {
			var rItem rawLedgerRow
			if err := rows.Scan(&rItem.id, &rItem.lYear, &rItem.seqNum, &rItem.serialNo, &rItem.itID,
				&rItem.cat, &rItem.name, &rItem.bUnit, &rItem.pUnit, &rItem.pSize,
				&rItem.tType, &rItem.tDate, &rItem.bQty, &rItem.pQty, &rItem.totAmt, &rItem.uPrice,
				&rItem.recip, &rItem.rmk, &rItem.createdAt); err == nil {
				rawList = append(rawList, rItem)
				distinctItems[rItem.itID] = true
			}
		}
		rows.Close()

		allTxDetail := make(map[int64]txReplayDetail)
		for itID := range distinctItems {
			_, txMap := replayConsumableMovingAvg(itID, "", "")
			for k, v := range txMap {
				allTxDetail[k] = v
			}
		}

		// 计算同年度每条流水之后的最小日期：若当前条目的日期晚于同年度后续条目的日期（如22日插在21日前面），则标记当前22日这一条！
		anomalyMsgMap := make(map[int64]string)
		minFutureDateByYear := make(map[int64]string)
		minFutureSerialByYear := make(map[int64]string)

		for i := len(rawList) - 1; i >= 0; i-- {
			cur := rawList[i]
			if minD, ok := minFutureDateByYear[cur.lYear]; ok {
				if cur.tDate > minD {
					anomalyMsgMap[cur.id] = fmt.Sprintf("日期 (%s) 晚于后续编号 %s (%s)，疑似日期倒挂", cur.tDate, minFutureSerialByYear[cur.lYear], minD)
				}
				if cur.tDate <= minD {
					minFutureDateByYear[cur.lYear] = cur.tDate
					minFutureSerialByYear[cur.lYear] = cur.serialNo
				}
			} else {
				minFutureDateByYear[cur.lYear] = cur.tDate
				minFutureSerialByYear[cur.lYear] = cur.serialNo
			}
		}

		list := make([]map[string]interface{}, 0, len(rawList))
		for _, rItem := range rawList {
			if category != "" && rItem.cat != category {
				continue
			}
			if itemID != "" && strconv.FormatInt(rItem.itID, 10) != itemID {
				continue
			}
			if txType != "" && rItem.tType != txType {
				continue
			}
			if yearStr != "" && strconv.FormatInt(rItem.lYear, 10) != yearStr {
				continue
			}
			if startDate != "" && rItem.tDate < startDate {
				continue
			}
			if endDate != "" && rItem.tDate > endDate {
				continue
			}

			det := allTxDetail[rItem.id]
			outCost := 0.0
			effUnitPrice := rItem.uPrice
			if rItem.tType == "OUT" {
				outCost = det.OutAmount
				effUnitPrice = det.OutUnitPrice
			}

			aMsg, hasAnomaly := anomalyMsgMap[rItem.id]

			spec := fmt.Sprintf("1%s=%d%s", rItem.pUnit, rItem.pSize, rItem.bUnit)
			if rItem.pUnit == "" {
				spec = fmt.Sprintf("1%s", rItem.bUnit)
			}

			list = append(list, map[string]interface{}{
				"id":           rItem.id,
				"ledger_year":  rItem.lYear,
				"seq_num":      rItem.seqNum,
				"serial_no":    rItem.serialNo,
				"item_id":      rItem.itID,
				"category":     rItem.cat,
				"item_name":    rItem.name,
				"base_unit":    rItem.bUnit,
				"pack_unit":    rItem.pUnit,
				"pack_size":    rItem.pSize,
				"pack_spec":    spec,
				"tx_type":      rItem.tType,
				"tx_date":      rItem.tDate,
				"before_qty":   det.BeforeQty,
				"base_qty":     rItem.bQty,
				"after_qty":    det.AfterQty,
				"pack_qty":     rItem.pQty,
				"total_amount": rItem.totAmt,
				"out_amount":   outCost,
				"unit_price":   round2(effUnitPrice),
				"recipient":    rItem.recip,
				"remark":       rItem.rmk,
				"date_anomaly": hasAnomaly,
				"anomaly_msg":  aMsg,
				"created_at":   rItem.createdAt,
			})
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "data": list})

	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		var req struct {
			ItemID         int64   `json:"item_id"`
			TxType         string  `json:"tx_type"`
			TxDate         string  `json:"tx_date"`
			BaseQty        float64 `json:"base_qty"`
			TotalAmount    float64 `json:"total_amount"`
			Recipient      string  `json:"recipient"`
			Remark         string  `json:"remark"`
			CustomSerialNo string  `json:"custom_serial_no"`
			AdminPasscode  string  `json:"admin_passcode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "参数格式错误"})
			return
		}
		if req.TxDate == "" {
			req.TxDate = todayStr()
		}
		if req.BaseQty <= 0 || math.Abs(req.BaseQty-math.Round(req.BaseQty)) > 0.0001 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "基本数量必须为大于 0 的整数（最小计量单位不可取小数）"})
			return
		}
		intBaseQty := int64(math.Round(req.BaseQty))

		if req.TxType != "IN" && req.TxType != "OUT" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "操作类型必须为入库(IN)或领用(OUT)"})
			return
		}

		var nature string
		var packSize int64
		err := db.QueryRow(`SELECT nature, pack_size FROM items WHERE id = ?`, req.ItemID).Scan(&nature, &packSize)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "未找到所选用品"})
			return
		}
		if nature != "consumable" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "复用品请通过类资产通道登记，不可走消耗品流水"})
			return
		}

		yearPart, err := strconv.Atoi(req.TxDate[:4])
		if err != nil {
			yearPart = time.Now().Year()
		}

		var maxCarryYear sql.NullInt64
		_ = db.QueryRow(`SELECT MAX(carry_year) FROM annual_carryover`).Scan(&maxCarryYear)
		isBackdate := false
		backdateReason := ""

		if maxCarryYear.Valid && int64(yearPart) <= maxCarryYear.Int64 {
			isBackdate = true
			backdateReason = fmt.Sprintf("%d年度已完成年度结转锁定", yearPart)
		}

		var latestDate sql.NullString
		_ = db.QueryRow(`SELECT MAX(tx_date) FROM consumable_ledger WHERE item_id = ?`, req.ItemID).Scan(&latestDate)
		if !isBackdate && latestDate.Valid && latestDate.String != "" && req.TxDate < latestDate.String {
			isBackdate = true
			backdateReason = fmt.Sprintf("该品种已存在更晚日期（%s）的连续流水记录，补登前期（%s）单据需管理员确认并可指定插入编号", latestDate.String, req.TxDate)
		}

		if strings.TrimSpace(req.CustomSerialNo) != "" {
			isBackdate = true
			if backdateReason == "" {
				backdateReason = "手动指定插入流水编号并重排连续序号"
			}
		}

		if isBackdate {
			if strings.TrimSpace(req.AdminPasscode) == "" {
				writeJSON(w, http.StatusConflict, map[string]interface{}{
					"success":       false,
					"need_override": true,
					"message":       fmt.Sprintf("时序/编号校验提示：%s！请输入管理员最高权限口令授权（可同时指定插入的流水编号，后续序号将自动连续重排）。", backdateReason),
				})
				return
			}
			if !verifyAdminPasscode(req.AdminPasscode) {
				writeJSON(w, http.StatusForbidden, map[string]interface{}{
					"success": false,
					"message": "授权失败：管理员最高权限口令不正确，拒绝补录或插号！",
				})
				return
			}
		}

		if req.TxType == "OUT" && strings.TrimSpace(req.Recipient) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "领用登记必须填写领用人"})
			return
		}

		desiredSeq := 999999999
		customNo := strings.TrimSpace(req.CustomSerialNo)
		if customNo != "" {
			cYear, cSeq, ok := parseConsumableSerialNo(customNo)
			if !ok {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "自定义流水编号格式错误，正确格式如：XH-2026-000002"})
				return
			}
			yearPart = cYear
			desiredSeq = cSeq
		}

		initPackQty := calcPackDisplay(intBaseQty, packSize)
		unitPrice := 0.0
		if req.TxType == "IN" && req.TotalAmount > 0 {
			unitPrice = round4(req.TotalAmount / float64(intBaseQty))
		}

		tx, err := db.Begin()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		defer tx.Rollback()

		now := nowStr()
		tmpSerial := fmt.Sprintf("INIT-TMP-%d", time.Now().UnixNano())
		res, err := tx.Exec(`
			INSERT INTO consumable_ledger (ledger_year, seq_num, serial_no, item_id, tx_type, tx_date, base_qty, pack_qty, total_amount, unit_price, recipient, remark, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			yearPart, desiredSeq, tmpSerial, req.ItemID, req.TxType, req.TxDate, intBaseQty, initPackQty, round2(req.TotalAmount), unitPrice, strings.TrimSpace(req.Recipient), strings.TrimSpace(req.Remark), now, now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		newID, _ := res.LastInsertId()

		finalSerialNo, err := resequenceConsumableYear(tx, yearPart, newID, desiredSeq)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		if err := tx.Commit(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}

		// 校验全周期（含中间插号时点）是否出现负库存，若出现负库存则回滚删除刚插入的记录
		snapAll, txMap := replayConsumableMovingAvg(req.ItemID, "", "")
		hasNeg := snapAll.StockQty < 0
		for _, d := range txMap {
			if d.HasNegative {
				hasNeg = true
				break
			}
		}
		if hasNeg {
			txClean, _ := db.Begin()
			if txClean != nil {
				_, _ = txClean.Exec(`DELETE FROM consumable_ledger WHERE id = ?`, newID)
				_, _ = resequenceConsumableYear(txClean, yearPart, 0, 0)
				_ = txClean.Commit()
			}
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"success": false,
				"message": fmt.Sprintf("库存不足！在当前编号时点或后续流转中可用库存不足以支取 %d（当前最新结存 %d）。", intBaseQty, snapAll.StockQty+intBaseQty),
			})
			return
		}

		reconcileItemPackQuantities(req.ItemID)

		var finalPackQty float64
		_ = db.QueryRow(`SELECT pack_qty FROM consumable_ledger WHERE id = ?`, newID).Scan(&finalPackQty)

		if isBackdate {
			writeAudit("特权补录/插号登记流水", finalSerialNo, fmt.Sprintf("单据日期:%s, 数量:%d, 原因:%s", req.TxDate, intBaseQty, backdateReason))
		}

		if req.TxType == "IN" {
			recalculateItemPriceRange(req.ItemID)
		} else {
			recordRecipient(req.Recipient)
		}

		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success":             true,
			"id":                  newID,
			"serial_no":           finalSerialNo,
			"reconciled_pack_qty": finalPackQty,
		})

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

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req struct {
		SerialNo      string  `json:"serial_no"`
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
	if req.BaseQty <= 0 || math.Abs(req.BaseQty-math.Round(req.BaseQty)) > 0.0001 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "基本数量必须为正整数"})
		return
	}
	intBaseQty := int64(math.Round(req.BaseQty))

	var itemID int64
	var oldYear, oldSeq int
	var oldSerialNo, txType, oldDate, oldRecip string
	var oldQty int64
	var oldAmt float64
	err = db.QueryRow(`SELECT item_id, ledger_year, seq_num, serial_no, tx_type, tx_date, base_qty, total_amount, recipient FROM consumable_ledger WHERE id = ?`, ledgerID).
		Scan(&itemID, &oldYear, &oldSeq, &oldSerialNo, &txType, &oldDate, &oldQty, &oldAmt, &oldRecip)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "流水记录不存在"})
		return
	}

	var packSize int64
	_ = db.QueryRow(`SELECT pack_size FROM items WHERE id = ?`, itemID).Scan(&packSize)
	if packSize <= 0 {
		packSize = 1
	}

	targetYear := oldYear
	targetSeq := oldSeq
	newSerialInput := strings.TrimSpace(req.SerialNo)
	if newSerialInput != "" && strings.ToUpper(newSerialInput) != oldSerialNo {
		pYear, pSeq, ok := parseConsumableSerialNo(newSerialInput)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "流水编号格式无效，请使用 XH-YYYY-000001 格式"})
			return
		}
		targetYear = pYear
		targetSeq = pSeq
	}

	packQty := calcPackDisplay(intBaseQty, packSize)
	unitPrice := 0.0
	if txType == "IN" && req.TotalAmount > 0 {
		unitPrice = round4(req.TotalAmount / float64(intBaseQty))
	}

	tx, err := db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer tx.Rollback()

	_, err = tx.Exec(`UPDATE consumable_ledger SET ledger_year = ?, tx_date = ?, base_qty = ?, pack_qty = ?, total_amount = ?, unit_price = ?, recipient = ?, remark = ?, updated_at = ? WHERE id = ?`,
		targetYear, req.TxDate, intBaseQty, packQty, round2(req.TotalAmount), unitPrice, strings.TrimSpace(req.Recipient), strings.TrimSpace(req.Remark), nowStr(), ledgerID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	// 若跨年份修改，先闭合原年份连续序号，再重排目标年份连续序号
	if targetYear != oldYear {
		if _, err := resequenceConsumableYear(tx, oldYear, 0, 0); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
	}
	finalSerialNo, err := resequenceConsumableYear(tx, targetYear, ledgerID, targetSeq)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

	// 校验修正并重排序号后全周期是否出现负库存，若冲突则自动恢复原状
	snapAll, txMap := replayConsumableMovingAvg(itemID, "", "")
	hasNeg := snapAll.StockQty < 0
	for _, d := range txMap {
		if d.HasNegative {
			hasNeg = true
			break
		}
	}
	if hasNeg {
		txRevert, _ := db.Begin()
		if txRevert != nil {
			oldUnit := 0.0
			if txType == "IN" && oldAmt > 0 && oldQty > 0 {
				oldUnit = round4(oldAmt / float64(oldQty))
			}
			_, _ = txRevert.Exec(`UPDATE consumable_ledger SET ledger_year = ?, tx_date = ?, base_qty = ?, total_amount = ?, unit_price = ?, recipient = ? WHERE id = ?`,
				oldYear, oldDate, oldQty, oldAmt, oldUnit, oldRecip, ledgerID)
			if targetYear != oldYear {
				_, _ = resequenceConsumableYear(txRevert, targetYear, 0, 0)
			}
			_, _ = resequenceConsumableYear(txRevert, oldYear, ledgerID, oldSeq)
			_ = txRevert.Commit()
		}
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "修改冲突：按此数量或编号调整后会导致某时点库存变为负数，已自动撤销本次修改！"})
		return
	}

	reconcileItemPackQuantities(itemID)

	if txType == "IN" {
		recalculateItemPriceRange(itemID)
	} else if req.Recipient != "" {
		recordRecipient(req.Recipient)
	}

	writeAudit("修正消耗品流水记录", finalSerialNo, fmt.Sprintf("原[编号:%s, 日期:%s, 数量:%d, 金额:%g] -> 新[编号:%s, 日期:%s, 数量:%d, 金额:%g]", oldSerialNo, oldDate, oldQty, oldAmt, finalSerialNo, req.TxDate, intBaseQty, req.TotalAmount))
	writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "serial_no": finalSerialNo, "message": "流水记录已修正，全年度序号已连续重排并闭合"})
}

// ==================== 8. 会计级期初/期末盘点（含本期入库包装数、移动加权在库价值与纯数字输出） ====================

type consumableBaseInfo struct {
	ID       int64
	Category string
	Name     string
	BaseUnit string
	PackUnit string
	PackSize int64
	MinPrice float64
	MaxPrice float64
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

	var baseList []consumableBaseInfo
	for rows.Next() {
		var b consumableBaseInfo
		if err := rows.Scan(&b.ID, &b.Category, &b.Name, &b.BaseUnit, &b.PackUnit, &b.PackSize, &b.MinPrice, &b.MaxPrice); err == nil {
			baseList = append(baseList, b)
		}
	}
	rows.Close()

	itemsList := make([]map[string]interface{}, 0, len(baseList))
	catSummaryMap := make(map[string]map[string]interface{})

	for idx, b := range baseList {
		snap, _ := replayConsumableMovingAvg(b.ID, startDate, endDate)

		openPack := calcPackDisplay(snap.OpeningQty, b.PackSize)
		inPack := calcPackDisplay(snap.PeriodInQty, b.PackSize)
		closePack := calcPackDisplay(snap.StockQty, b.PackSize)
		outPack := round2(openPack + inPack - closePack)
		if snap.PeriodOutQty == 0 {
			outPack = 0
		}

		spec := fmt.Sprintf("1%s=%d%s", b.PackUnit, b.PackSize, b.BaseUnit)
		if b.PackUnit == "" {
			spec = fmt.Sprintf("1%s", b.BaseUnit)
		}

		rowObj := map[string]interface{}{
			"seq":               idx + 1,
			"item_id":           b.ID,
			"category":          b.Category,
			"item_name":         b.Name,
			"base_unit":         b.BaseUnit,
			"pack_unit":         b.PackUnit,
			"pack_size":         b.PackSize,
			"pack_spec":         spec,
			"opening_base_qty":  snap.OpeningQty,
			"opening_pack_qty":  openPack,
			"in_base_qty":       snap.PeriodInQty,
			"in_pack_qty":       inPack,
			"out_base_qty":      snap.PeriodOutQty,
			"out_pack_qty":      outPack,
			"closing_base_qty":  snap.StockQty,
			"closing_pack_qty":  closePack,
			"period_in_amount":  snap.PeriodInValue,
			"period_out_amount": snap.PeriodOutValue,
			"closing_value":     snap.StockValue,
			"moving_avg_price":  round2(snap.MovingUnitPrice),
			"price_range":       formatPriceRange(b.MinPrice, b.MaxPrice),
		}
		itemsList = append(itemsList, rowObj)

		if _, ok := catSummaryMap[b.Category]; !ok {
			catSummaryMap[b.Category] = map[string]interface{}{
				"category":            b.Category,
				"item_count":          0,
				"total_closing_value": 0.0,
				"total_in_amt":        0.0,
				"active_skus":         0,
			}
		}
		catSummaryMap[b.Category]["item_count"] = catSummaryMap[b.Category]["item_count"].(int) + 1
		catSummaryMap[b.Category]["total_closing_value"] = round2(catSummaryMap[b.Category]["total_closing_value"].(float64) + snap.StockValue)
		catSummaryMap[b.Category]["total_in_amt"] = round2(catSummaryMap[b.Category]["total_in_amt"].(float64) + snap.PeriodInValue)
		if snap.StockQty > 0 {
			catSummaryMap[b.Category]["active_skus"] = catSummaryMap[b.Category]["active_skus"].(int) + 1
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
		return fmt.Sprintf("%.2f", minP)
	}
	return fmt.Sprintf("%.2f ~ %.2f", minP, maxP)
}

// ==================== 8.5 采购价格正态/频率分布直方图分析接口 ====================

func handleItemPriceDistribution(w http.ResponseWriter, r *http.Request) {
	itemIDStr := r.URL.Query().Get("item_id")
	itemID, err := strconv.ParseInt(itemIDStr, 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "无效用品ID"})
		return
	}

	var cat, name, bUnit string
	if err := db.QueryRow(`SELECT category, name, base_unit FROM items WHERE id = ?`, itemID).Scan(&cat, &name, &bUnit); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "用品不存在"})
		return
	}

	rows, err := db.Query(`
		SELECT tx_date, base_qty, total_amount, unit_price
		FROM consumable_ledger
		WHERE item_id = ? AND tx_type = 'IN' AND unit_price > 0
		ORDER BY unit_price ASC`, itemID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer rows.Close()

	type priceRecord struct {
		Date      string  `json:"date"`
		Qty       int64   `json:"qty"`
		TotalAmt  float64 `json:"total_amount"`
		UnitPrice float64 `json:"unit_price"`
	}
	var records []priceRecord
	var prices []float64
	var weightedSum float64
	var totalQty int64

	for rows.Next() {
		var rec priceRecord
		if err := rows.Scan(&rec.Date, &rec.Qty, &rec.TotalAmt, &rec.UnitPrice); err == nil {
			records = append(records, rec)
			prices = append(prices, rec.UnitPrice)
			weightedSum += rec.TotalAmt
			totalQty += rec.Qty
		}
	}

	if len(prices) == 0 {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true, "has_data": false, "item_name": name, "category": cat, "base_unit": bUnit,
		})
		return
	}

	sort.Float64s(prices)
	minP := prices[0]
	maxP := prices[len(prices)-1]

	var sum float64
	for _, p := range prices {
		sum += p
	}
	mean := sum / float64(len(prices))
	weightedMean := weightedSum / float64(totalQty)
	median := prices[len(prices)/2]
	if len(prices)%2 == 0 {
		median = (prices[len(prices)/2-1] + prices[len(prices)/2]) / 2
	}

	var sqDiffSum float64
	for _, p := range prices {
		sqDiffSum += (p - mean) * (p - mean)
	}
	stdDev := math.Sqrt(sqDiffSum / float64(len(prices)))
	reasonableLow := math.Max(minP, round2(mean-stdDev))
	reasonableHigh := math.Min(maxP, round2(mean+stdDev))

	binCount := 5
	type histBin struct {
		Label      string  `json:"label"`
		Low        float64 `json:"low"`
		High       float64 `json:"high"`
		BatchCount int     `json:"batch_count"`
		TotalQty   int64   `json:"total_qty"`
		NormalPdf  float64 `json:"normal_pdf"`
	}
	bins := make([]histBin, binCount)

	if math.Abs(maxP-minP) < 0.01 {
		bins = []histBin{{
			Label:      fmt.Sprintf("%.2f", minP),
			Low:        minP,
			High:       maxP,
			BatchCount: len(records),
			TotalQty:   totalQty,
			NormalPdf:  1.0,
		}}
	} else {
		step := (maxP - minP) / float64(binCount)
		for i := 0; i < binCount; i++ {
			low := minP + float64(i)*step
			high := minP + float64(i+1)*step
			mid := (low + high) / 2
			pdf := 1.0
			if stdDev > 0 {
				pdf = math.Exp(-0.5 * math.Pow((mid-mean)/stdDev, 2))
			}
			bins[i] = histBin{
				Label:     fmt.Sprintf("%.2f~%.2f", low, high),
				Low:       round2(low),
				High:      round2(high),
				NormalPdf: round4(pdf),
			}
		}
		for _, rec := range records {
			idx := int((rec.UnitPrice - minP) / step)
			if idx >= binCount {
				idx = binCount - 1
			}
			if idx < 0 {
				idx = 0
			}
			bins[idx].BatchCount++
			bins[idx].TotalQty += rec.Qty
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"success":         true,
		"has_data":        true,
		"item_name":       name,
		"category":        cat,
		"base_unit":       bUnit,
		"batch_total":     len(records),
		"min_price":       round2(minP),
		"max_price":       round2(maxP),
		"mean_price":      round2(mean),
		"weighted_price":  round2(weightedMean),
		"median_price":    round2(median),
		"std_dev":         round2(stdDev),
		"reasonable_low":  reasonableLow,
		"reasonable_high": reasonableHigh,
		"bins":            bins,
		"records":         records,
	})
}

// ==================== 8.6 年度结转与取消年度结转 ====================

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

	if r.Method == http.MethodDelete {
		yearStr := r.URL.Query().Get("carry_year")
		pass := r.URL.Query().Get("admin_passcode")
		carryYear, err := strconv.Atoi(yearStr)
		if err != nil || carryYear < 2000 || carryYear > 2100 {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "请指定有效的取消结转年份"})
			return
		}
		if !verifyAdminPasscode(pass) {
			writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：取消年度结转需要管理员最高权限口令！"})
			return
		}
		var exists int
		_ = db.QueryRow(`SELECT COUNT(1) FROM annual_carryover WHERE carry_year = ?`, carryYear).Scan(&exists)
		if exists == 0 {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": fmt.Sprintf("%d 年度尚未执行过结转", carryYear)})
			return
		}
		_, err = db.Exec(`DELETE FROM annual_carryover WHERE carry_year = ?`, carryYear)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
		writeAudit("取消年度结转", fmt.Sprintf("%d年度", carryYear), "管理员已撤销该年度结转锁定快照")
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"success": true,
			"message": fmt.Sprintf("已成功取消 %d 年度结转记录，系统已重新计算连续期初与在库余值。", carryYear),
		})
		return
	}

	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]interface{}{"success": false})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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

	closingQtyMap := make(map[int64]int64, len(itemIDs))
	closingValMap := make(map[int64]float64, len(itemIDs))
	for _, itID := range itemIDs {
		snap, _ := replayConsumableMovingAvg(itID, "", yearEndDate)
		closingQtyMap[itID] = snap.StockQty
		closingValMap[itID] = snap.StockValue
	}

	now := nowStr()
	tx, err := db.Begin()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}
	defer tx.Rollback()

	for _, itID := range itemIDs {
		_, err := tx.Exec(`
			INSERT INTO annual_carryover (carry_year, item_id, closing_base_qty, closing_value, created_at)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT(carry_year, item_id) DO UPDATE SET closing_base_qty = excluded.closing_base_qty, closing_value = excluded.closing_value, created_at = excluded.created_at`,
			req.CarryYear, itID, closingQtyMap[itID], closingValMap[itID], now)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
			return
		}
	}

	if err := tx.Commit(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
		return
	}

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

// ==================== 9. 复用品（类资产全生命周期管理、预计处置日期与取消报废） ====================

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
		q += ` ORDER BY ra.asset_no ASC, ra.id ASC`

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
				"unit_price":      round2(uPrice),
				"expected_years":  expYears,
				"disposal_date":   calcDisposalDate(pDate, expYears),
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
		if err := r.ParseMultipartForm(30 << 20); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "表单解析失败"})
			return
		}
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		itemID, _ := strconv.ParseInt(r.FormValue("item_id"), 10, 64)
		qty, _ := strconv.Atoi(r.FormValue("quantity"))
		if qty <= 0 {
			qty = 1
		}
		totalAmt, _ := strconv.ParseFloat(r.FormValue("total_amount"), 64)
		expectedYears, _ := strconv.Atoi(r.FormValue("expected_years"))
		if expectedYears <= 0 {
			expectedYears = 10
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
				fmt.Sprintf("新购入库建档，二级品名：%s，预计使用：%d年（预计处置：%s），单价：%.2f", itemSub, expectedYears, calcDisposalDate(purchaseDate, expectedYears), unitPrice), savedRelImg, now)

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
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
		action := r.FormValue("action")
		actionDate := strings.TrimSpace(r.FormValue("action_date"))
		if actionDate == "" {
			actionDate = todayStr()
		}
		user := strings.TrimSpace(r.FormValue("user"))
		remark := strings.TrimSpace(r.FormValue("remark"))
		adminPass := strings.TrimSpace(r.FormValue("admin_passcode"))

		var assetNo, curStatus, curUser, itName string
		err := db.QueryRow(`SELECT ra.asset_no, ra.status, ra.current_user, i.name FROM reusable_assets ra JOIN items i ON ra.item_id = i.id WHERE ra.id = ?`, assetID).
			Scan(&assetNo, &curStatus, &curUser, &itName)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{"success": false, "message": "未找到该资产"})
			return
		}

		if action == "UNSCRAP" {
			if curStatus != "报废" {
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "该资产当前未处于报废状态，无需取消报废"})
				return
			}
			if !verifyAdminPasscode(adminPass) {
				writeJSON(w, http.StatusForbidden, map[string]interface{}{"success": false, "message": "验证失败：取消资产报废需要管理员最高权限口令！"})
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
			_, _ = tx.Exec(`UPDATE reusable_assets SET status = '闲置', current_user = '', scrap_date = '', updated_at = ? WHERE id = ?`, now, assetID)
			_, _ = tx.Exec(`INSERT INTO reusable_logs (log_no, asset_id, action_type, action_date, operator_or_user, detail, created_at) VALUES (?,?,?,?,?,?,?)`,
				logNo, assetID, "取消报废", actionDate, "管理员", fmt.Sprintf("管理员特权取消报废，资产恢复为【闲置】状态。%s", remark), now)
			if err := tx.Commit(); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"success": false, "message": err.Error()})
				return
			}
			writeAudit("取消复用品报废", assetNo, fmt.Sprintf("品名:%s, 恢复为闲置状态。%s", itName, remark))
			writeJSON(w, http.StatusOK, map[string]interface{}{"success": true, "log_no": logNo, "message": "已取消报废并恢复为闲置可用状态"})
			return
		}

		if curStatus == "报废" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"success": false, "message": "该资产已登记报废，不可再进行领用或变更操作！如需恢复请点击【取消报废】。"})
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
		defer func() {
			if r.MultipartForm != nil {
				_ = r.MultipartForm.RemoveAll()
			}
		}()
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
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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

// ==================== 11. Excel 专业导出（右上角标注金额单位：元 + 居中/金额右对齐 + 前后结存数量） ====================

func handleExportExcel(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("mode")
	startDate := r.URL.Query().Get("start_date")
	endDate := r.URL.Query().Get("end_date")
	customOrg := strings.TrimSpace(r.URL.Query().Get("org_name"))
	customTitle := strings.TrimSpace(r.URL.Query().Get("report_title"))

	if startDate == "" {
		startDate = fmt.Sprintf("%d-01-01", time.Now().Year())
	}
	if endDate == "" {
		endDate = todayStr()
	}
	if customOrg == "" {
		cfgMutex.RLock()
		customOrg = appCfg.OrgName
		cfgMutex.RUnlock()
	}

	f := excelize.NewFile()
	defer f.Close()

	titleStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 15, Family: "Microsoft YaHei"},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center"},
	})
	metaLeftStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Family: "Microsoft YaHei", Color: "#334155"},
		Alignment: &excelize.Alignment{Horizontal: "left", Vertical: "center"},
	})
	metaRightStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 10, Family: "Microsoft YaHei", Color: "#0F172A"},
		Alignment: &excelize.Alignment{Horizontal: "right", Vertical: "center"},
	})
	headerStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Bold: true, Size: 10, Family: "Microsoft YaHei"},
		Fill:      excelize.Fill{Type: "pattern", Color: []string{"#E2E8F0"}, Pattern: 1},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "94A3B8", Style: 1},
			{Type: "right", Color: "94A3B8", Style: 1},
			{Type: "top", Color: "94A3B8", Style: 1},
			{Type: "bottom", Color: "94A3B8", Style: 2},
		},
	})
	centerCellStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Family: "Microsoft YaHei"},
		Alignment: &excelize.Alignment{Horizontal: "center", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "CBD5E1", Style: 1},
			{Type: "right", Color: "CBD5E1", Style: 1},
			{Type: "top", Color: "CBD5E1", Style: 1},
			{Type: "bottom", Color: "CBD5E1", Style: 1},
		},
	})
	moneyCellStyle, _ := f.NewStyle(&excelize.Style{
		Font:      &excelize.Font{Size: 10, Family: "Microsoft YaHei"},
		Alignment: &excelize.Alignment{Horizontal: "right", Vertical: "center", WrapText: true},
		Border: []excelize.Border{
			{Type: "left", Color: "CBD5E1", Style: 1},
			{Type: "right", Color: "CBD5E1", Style: 1},
			{Type: "top", Color: "CBD5E1", Style: 1},
			{Type: "bottom", Color: "CBD5E1", Style: 1},
		},
	})

	setupSheetHeader := func(sheet, title, subMeta string, colCount int) {
		endCol, _ := excelize.ColumnNumberToName(colCount)
		midColNum := colCount - 2
		if midColNum < 1 {
			midColNum = 1
		}
		midCol, _ := excelize.ColumnNumberToName(midColNum)
		rightStartCol, _ := excelize.ColumnNumberToName(midColNum + 1)

		_ = f.MergeCell(sheet, "A1", endCol+"1")
		_ = f.SetCellValue(sheet, "A1", title)
		_ = f.SetCellStyle(sheet, "A1", endCol+"1", titleStyle)
		_ = f.SetRowHeight(sheet, 1, 28)

		_ = f.MergeCell(sheet, "A2", midCol+"2")
		_ = f.SetCellValue(sheet, "A2", subMeta)
		_ = f.SetCellStyle(sheet, "A2", midCol+"2", metaLeftStyle)

		if colCount > 2 {
			_ = f.MergeCell(sheet, rightStartCol+"2", endCol+"2")
			_ = f.SetCellValue(sheet, rightStartCol+"2", "金额单位：元")
			_ = f.SetCellStyle(sheet, rightStartCol+"2", endCol+"2", metaRightStyle)
		}
		_ = f.SetRowHeight(sheet, 2, 20)
		_ = f.SetRowHeight(sheet, 3, 28)

		orient := "landscape"
		fitToWidth := 1
		fitToHeight := 0
		_ = f.SetPageLayout(sheet, &excelize.PageLayoutOptions{
			Orientation: &orient,
			FitToWidth:  &fitToWidth,
			FitToHeight: &fitToHeight,
		})
		_ = f.SetSheetProps(sheet, &excelize.SheetPropsOptions{FitToPage: boolPtr(true)})
	}

	fileName := "办公用品台账导出.xlsx"

	switch mode {
	case "consumable_ledger":
		sheet := "消耗品接续流水账"
		_ = f.SetSheetName("Sheet1", sheet)
		if customTitle == "" {
			customTitle = "办公消耗品入库与领用接续流水账"
		}
		headers := []string{"流水编号", "登记日期", "用品分类", "品名", "基本单位", "包装单位", "包装规格", "操作类型", "入库/领用前数量", "基本数量", "入库/领用后数量", "倒挤包装数", "采购/领用金额", "单价", "领用人", "备注"}
		moneyCols := map[int]bool{12: true, 13: true}
		setupSheetHeader(sheet, customTitle, fmt.Sprintf("单位/部门：%s    |    统计区间：%s 至 %s", customOrg, startDate, endDate), len(headers))
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 3)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`
			SELECT cl.id, cl.item_id, cl.serial_no, cl.tx_date, i.category, i.name, i.base_unit, i.pack_unit, i.pack_size, cl.tx_type, cl.base_qty, cl.pack_qty, cl.total_amount, cl.unit_price, cl.recipient, cl.remark
			FROM consumable_ledger cl JOIN items i ON cl.item_id = i.id
			WHERE cl.tx_date >= ? AND cl.tx_date <= ?
			ORDER BY cl.ledger_year ASC, cl.seq_num ASC, cl.id ASC`, startDate, endDate)

		type expLedger struct {
			id, itID, bQty, pSize                                    int64
			sNo, tDate, cat, name, bUnit, pUnit, tType, recip, rmk   string
			pQty, tAmt, uPrice                                       float64
		}
		var list []expLedger
		itemsSet := make(map[int64]bool)
		if rows != nil {
			for rows.Next() {
				var e expLedger
				if err := rows.Scan(&e.id, &e.itID, &e.sNo, &e.tDate, &e.cat, &e.name, &e.bUnit, &e.pUnit, &e.pSize, &e.tType, &e.bQty, &e.pQty, &e.tAmt, &e.uPrice, &e.recip, &e.rmk); err == nil {
					list = append(list, e)
					itemsSet[e.itID] = true
				}
			}
			rows.Close()
		}
		allDetails := make(map[int64]txReplayDetail)
		for itID := range itemsSet {
			_, m := replayConsumableMovingAvg(itID, "", "")
			for k, v := range m {
				allDetails[k] = v
			}
		}

		rIdx := 4
		for _, e := range list {
			typeZh := "入库"
			amtShow := round2(e.tAmt)
			unitShow := round2(e.uPrice)
			det := allDetails[e.id]
			if e.tType == "OUT" {
				typeZh = "领用"
				amtShow = det.OutAmount
				unitShow = round2(det.OutUnitPrice)
			}
			spec := fmt.Sprintf("1%s=%d%s", e.pUnit, e.pSize, e.bUnit)
			if e.pUnit == "" {
				spec = fmt.Sprintf("1%s", e.bUnit)
			}
			vals := []interface{}{e.sNo, e.tDate, e.cat, e.name, e.bUnit, e.pUnit, spec, typeZh, det.BeforeQty, e.bQty, det.AfterQty, e.pQty, amtShow, unitShow, e.recip, e.rmk}
			for cIdx, v := range vals {
				cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
				_ = f.SetCellValue(sheet, cell, v)
				st := centerCellStyle
				if moneyCols[cIdx] {
					st = moneyCellStyle
				}
				_ = f.SetCellStyle(sheet, cell, cell, st)
			}
			rIdx++
		}
		fileName = fmt.Sprintf("消耗品接续流水账_%s至%s.xlsx", startDate, endDate)

	case "reusable_assets":
		sheet := "复用品类资产台账"
		_ = f.SetSheetName("Sheet1", sheet)
		if customTitle == "" {
			customTitle = "企业复用品（类资产）独立编码盘点台账"
		}
		headers := []string{"资产编号", "用品分类", "标准品名", "二级品名/位置", "购置日期", "购置单价", "预计年限(年)", "预计处置日期", "当前状态", "当前使用人", "报废日期", "备注"}
		moneyCols := map[int]bool{5: true}
		setupSheetHeader(sheet, customTitle, fmt.Sprintf("单位/部门：%s    |    导出日期：%s", customOrg, todayStr()), len(headers))
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 3)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`
			SELECT ra.asset_no, i.category, i.name, ra.sub_name, ra.purchase_date, ra.unit_price, ra.expected_years, ra.status, ra.current_user, ra.scrap_date, ra.remark
			FROM reusable_assets ra JOIN items i ON ra.item_id = i.id ORDER BY ra.asset_no ASC, ra.id ASC`)
		rIdx := 4
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var aNo, cat, name, sub, pDate, st, user, sDate, rmk string
				var uPrice float64
				var expY int
				_ = rows.Scan(&aNo, &cat, &name, &sub, &pDate, &uPrice, &expY, &st, &user, &sDate, &rmk)
				vals := []interface{}{aNo, cat, name, sub, pDate, round2(uPrice), expY, calcDisposalDate(pDate, expY), st, user, sDate, rmk}
				for cIdx, v := range vals {
					cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
					_ = f.SetCellValue(sheet, cell, v)
					stCell := centerCellStyle
					if moneyCols[cIdx] {
						stCell = moneyCellStyle
					}
					_ = f.SetCellStyle(sheet, cell, cell, stCell)
				}
				rIdx++
			}
		}
		fileName = fmt.Sprintf("复用品类资产盘点台账_%s.xlsx", todayStr())

	case "reusable_single_logs":
		assetID, _ := strconv.ParseInt(r.URL.Query().Get("asset_id"), 10, 64)
		var assetNo, cat, itName, subName, pDate, st, curUser string
		var uPrice float64
		var expY int
		_ = db.QueryRow(`
			SELECT ra.asset_no, i.category, i.name, ra.sub_name, ra.purchase_date, ra.unit_price, ra.expected_years, ra.status, ra.current_user
			FROM reusable_assets ra JOIN items i ON ra.item_id = i.id WHERE ra.id = ?`, assetID).
			Scan(&assetNo, &cat, &itName, &subName, &pDate, &uPrice, &expY, &st, &curUser)

		sheet := "资产全生命周期履历"
		_ = f.SetSheetName("Sheet1", sheet)
		if customTitle == "" {
			customTitle = fmt.Sprintf("复用品（类资产）全生命周期履历表 - %s (%s)", assetNo, itName)
		}
		headers := []string{"流转编号", "操作日期", "流转动作", "相关人员", "详情说明", "附件路径"}
		subInfo := fmt.Sprintf("单位：%s | 资产编号：%s | 品名：%s(%s) | 购置单价：%.2f | 预计处置：%s | 当前状态：%s",
			customOrg, assetNo, itName, subName, round2(uPrice), calcDisposalDate(pDate, expY), st)
		setupSheetHeader(sheet, customTitle, subInfo, len(headers))
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 3)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`SELECT log_no, action_date, action_type, operator_or_user, detail, attachment_path FROM reusable_logs WHERE asset_id = ? ORDER BY id ASC`, assetID)
		rIdx := 4
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var lNo, aDate, aType, opUser, det, att string
				_ = rows.Scan(&lNo, &aDate, &aType, &opUser, &det, &att)
				vals := []interface{}{lNo, aDate, aType, opUser, det, att}
				for cIdx, v := range vals {
					cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
					_ = f.SetCellValue(sheet, cell, v)
					_ = f.SetCellStyle(sheet, cell, cell, centerCellStyle)
				}
				rIdx++
			}
		}
		fileName = fmt.Sprintf("资产履历_%s_%s.xlsx", assetNo, itName)

	default:
		sheet := "消耗品盘点统计表"
		_ = f.SetSheetName("Sheet1", sheet)
		if customTitle == "" {
			customTitle = "办公消耗品期初/期末盘点与移动加权在库价值报表"
		}
		headers := []string{
			"序号", "用品分类", "品名", "基本单位", "包装单位", "包装规格",
			"期初结存(基本数量)", "期初结存(包装数量)",
			"本期入库(基本数量)", "本期入库(包装数量)",
			"本期领用(基本数量)",
			"期末结存(基本数量)", "期末结存(包装数量)",
			"平均价", "当前在库价值", "采购价格分布",
		}
		moneyCols := map[int]bool{13: true, 14: true}
		setupSheetHeader(sheet, customTitle, fmt.Sprintf("单位/部门：%s    |    统计区间：%s 至 %s", customOrg, startDate, endDate), len(headers))
		for i, h := range headers {
			cell, _ := excelize.CoordinatesToCellName(i+1, 3)
			_ = f.SetCellValue(sheet, cell, h)
			_ = f.SetCellStyle(sheet, cell, cell, headerStyle)
		}
		rows, _ := db.Query(`SELECT id, category, name, base_unit, pack_unit, pack_size, min_price, max_price FROM items WHERE nature = 'consumable' ORDER BY category ASC, id ASC`)
		var baseList []consumableBaseInfo
		if rows != nil {
			for rows.Next() {
				var b consumableBaseInfo
				if err := rows.Scan(&b.ID, &b.Category, &b.Name, &b.BaseUnit, &b.PackUnit, &b.PackSize, &b.MinPrice, &b.MaxPrice); err == nil {
					baseList = append(baseList, b)
				}
			}
			rows.Close()
		}

		rIdx := 4
		for idx, b := range baseList {
			snap, _ := replayConsumableMovingAvg(b.ID, startDate, endDate)
			spec := fmt.Sprintf("1%s=%d%s", b.PackUnit, b.PackSize, b.BaseUnit)
			if b.PackUnit == "" {
				spec = fmt.Sprintf("1%s", b.BaseUnit)
			}
			vals := []interface{}{
				idx + 1, b.Category, b.Name, b.BaseUnit, b.PackUnit, spec,
				snap.OpeningQty, calcPackDisplay(snap.OpeningQty, b.PackSize),
				snap.PeriodInQty, calcPackDisplay(snap.PeriodInQty, b.PackSize),
				snap.PeriodOutQty,
				snap.StockQty, calcPackDisplay(snap.StockQty, b.PackSize),
				round2(snap.MovingUnitPrice), snap.StockValue,
				formatPriceRange(b.MinPrice, b.MaxPrice),
			}
			for cIdx, v := range vals {
				cell, _ := excelize.CoordinatesToCellName(cIdx+1, rIdx)
				_ = f.SetCellValue(sheet, cell, v)
				stCell := centerCellStyle
				if moneyCols[cIdx] {
					stCell = moneyCellStyle
				}
				_ = f.SetCellStyle(sheet, cell, cell, stCell)
			}
			rIdx++
		}
		fileName = fmt.Sprintf("消耗品在库价值盘点表_%s至%s.xlsx", startDate, endDate)
	}

	w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, fileName))
	_ = f.Write(w)
}

func boolPtr(b bool) *bool { return &b }

// ==================== 12. 路由装配与 HTTP 服务热重启 ====================

func buildHTTPMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth/status", handleAuthStatus)
	mux.HandleFunc("/api/auth/login", handleAuthLogin)
	mux.HandleFunc("/api/settings", handleUpdateSettings)

	mux.HandleFunc("/api/items", handleItems)
	mux.HandleFunc("/api/items/", handleItemByID)
	mux.HandleFunc("/api/items/price-distribution", handleItemPriceDistribution)

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
		Handler:           buildHTTPMux(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
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

// ==================== 13. 原生 32x32 高清托盘图标生成器 + 系统托盘入口 ====================

func buildTrayIconBytes() []byte {
	const w, h = 32, 32
	var buf bytes.Buffer

	_ = binary.Write(&buf, binary.LittleEndian, uint16(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))

	pixelDataSize := w * h * 4
	maskDataSize := (w / 8) * h
	dibSize := 40 + pixelDataSize + maskDataSize

	buf.WriteByte(w)
	buf.WriteByte(h)
	buf.WriteByte(0)
	buf.WriteByte(0)
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dibSize))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(22))

	_ = binary.Write(&buf, binary.LittleEndian, uint32(40))
	_ = binary.Write(&buf, binary.LittleEndian, int32(w))
	_ = binary.Write(&buf, binary.LittleEndian, int32(h*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(32))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(pixelDataSize+maskDataSize))
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))
	_ = binary.Write(&buf, binary.LittleEndian, int32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0))

	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			b, g, r, a := uint8(0), uint8(0), uint8(0), uint8(0)
			inBox := x >= 2 && x <= 29 && y >= 2 && y <= 29
			isCorner := (x <= 3 && y <= 3) || (x >= 28 && y <= 3) || (x <= 3 && y >= 28) || (x >= 28 && y >= 28)
			if inBox && !isCorner {
				r, g, b, a = 37, 99, 235, 255
				if y >= 6 && y <= 8 && x >= 7 && x <= 24 {
					r, g, b = 147, 197, 253
				}
				if (y >= 12 && y <= 13 && x >= 7 && x <= 24) ||
					(y >= 17 && y <= 18 && x >= 7 && x <= 24) ||
					(y >= 22 && y <= 23 && x >= 7 && x <= 19) {
					r, g, b = 255, 255, 255
				}
			}
			buf.Write([]byte{b, g, r, a})
		}
	}
	buf.Write(make([]byte, maskDataSize))
	return buf.Bytes()
}

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
	systray.SetIcon(buildTrayIconBytes())
	systray.SetTitle("办公用品台账")

	exeName := "OfficeLedgerManager.exe"
	if p, err := os.Executable(); err == nil {
		exeName = filepath.Base(p)
	}
	pid := os.Getpid()
	systray.SetTooltip(fmt.Sprintf("企业办公用品与类资产台账系统\n进程: %s (PID: %d)\n端口: %d", exeName, pid, currentPort))

	mInfo := systray.AddMenuItem(fmt.Sprintf("🟢 运行中: %s (PID:%d | 端口:%d)", exeName, pid, currentPort), "在任务管理器【详细信息】标签页按此名称定位后台进程")
	mInfo.Disable()
	systray.AddSeparator()

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
