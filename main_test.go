package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/xuri/excelize/v2"
)

func TestFullBusinessRegression(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "office-ledger-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	_ = os.Setenv("LEDGER_DATA_ROOT", tmpDir)
	if err := initDirectoriesAndConfig(); err != nil {
		t.Fatalf("初始化目录失败: %v", err)
	}
	if err := initDatabase(); err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	defer db.Close()

	if err := startHTTPServer(0); err != nil {
		t.Fatalf("启动测试服务器失败: %v", err)
	}
	defer httpSrv.Close()

	baseURL := fmt.Sprintf("http://127.0.0.1:%d", currentPort)

	// ==================== 1. 基础字典新增与防重 ====================
	body1, _ := json.Marshal(map[string]interface{}{
		"category": "办公文具", "name": "测试中性笔", "nature": "consumable",
		"base_unit": "支", "pack_unit": "盒", "pack_size": 12,
	})
	resp1, err := http.Post(baseURL+"/api/items", "application/json", bytes.NewReader(body1))
	if err != nil || resp1.StatusCode != 200 {
		t.Fatalf("新增用品失败: %v", err)
	}
	var createItemRes map[string]interface{}
	_ = json.NewDecoder(resp1.Body).Decode(&createItemRes)
	itemID := int64(createItemRes["id"].(float64))

	// ==================== 2. 基本数量强制整数 + 移动加权平均成本 + 包装尾差自动倒挤 ====================
	// 2.1 小数基本数量必须拦截
	decPayload, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 3.96, "total_amount": 10,
	})
	respDec, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(decPayload))
	if respDec.StatusCode != 400 {
		t.Fatalf("小数基本数量必须被拦截，实际状态码: %d", respDec.StatusCode)
	}

	// 2.2 首批入库 12 支（1盒），总价 24 元（单价 2 元）
	in1, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 12, "total_amount": 24,
	})
	_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(in1))

	// 2.3 二批入库 12 支（1盒），总价 48 元（单价 4 元），此时共 24 支，移动加权均价 3 元，总价值 72 元
	in2, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-15", "base_qty": 12, "total_amount": 48,
	})
	_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(in2))

	// 2.4 分 6 次每次领用 4 支（共 24 支全部领完，每次显示 0.33 盒）
	for i := 1; i <= 6; i++ {
		outP, _ := json.Marshal(map[string]interface{}{
			"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-20", "base_qty": 4, "recipient": fmt.Sprintf("领用人%d", i),
		})
		_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(outP))
	}

	// 验证全部领完后：期末结存基本数=0，包装数自动倒挤=0，在库价值=0（无任何尾差残留）
	respInv, _ := http.Get(fmt.Sprintf("%s/api/consumables/inventory?start_date=2026-03-01&end_date=2026-03-31&item_id=%d", baseURL, itemID))
	var invRes map[string]interface{}
	_ = json.NewDecoder(respInv.Body).Decode(&invRes)
	itStat := invRes["items"].([]interface{})[0].(map[string]interface{})
	if itStat["closing_base_qty"].(float64) != 0 || itStat["closing_pack_qty"].(float64) != 0 || itStat["closing_value"].(float64) != 0 {
		t.Fatalf("全部领完后应无任何包装与金额尾差，实际: %+v", itStat)
	}

	// ==================== 3. 采购价格正态/频率分布直方图接口 ====================
	respDist, _ := http.Get(fmt.Sprintf("%s/api/items/price-distribution?item_id=%d", baseURL, itemID))
	var distRes map[string]interface{}
	_ = json.NewDecoder(respDist.Body).Decode(&distRes)
	if distRes["has_data"] != true || distRes["mean_price"].(float64) != 3.0 {
		t.Fatalf("价格分布分析计算异常: %+v", distRes)
	}

	// ==================== 4. 连续登记时序校验（禁止普通权限倒插补登前期记录） ====================
	// 已有 2026-03-20 记录，尝试补录 2026-03-18 入库（无管理员口令）必须返回 409
	backInNoPass, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-18", "base_qty": 10, "total_amount": 30,
	})
	respBack1, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(backInNoPass))
	if respBack1.StatusCode != 409 {
		t.Fatalf("在已有更晚日期记录时补登前期记录应被 409 拦截，实际: %d", respBack1.StatusCode)
	}

	// 携带管理员最高权限口令授权补录必须成功
	backInWithPass, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-18", "base_qty": 10, "total_amount": 30, "admin_passcode": "888888",
	})
	respBack2, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(backInWithPass))
	if respBack2.StatusCode != 200 {
		t.Fatalf("管理员特权补录前期记录应成功，实际: %d", respBack2.StatusCode)
	}

	// ==================== 5. 年度结转锁定与撤销年度结转 ====================
	coPayload, _ := json.Marshal(map[string]interface{}{"carry_year": 2026, "admin_passcode": "888888"})
	_, _ = http.Post(baseURL+"/api/carryover", "application/json", bytes.NewReader(coPayload))

	// 结转后尝试登记 2026 年流水（无口令）必须被拦截
	lockedYearTx, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-12-25", "base_qty": 5, "total_amount": 15,
	})
	respLockedYear, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(lockedYearTx))
	if respLockedYear.StatusCode != 409 {
		t.Fatalf("已结转年度补录应被 409 拦截，实际: %d", respLockedYear.StatusCode)
	}

	// 管理员撤销 2026 年度结转
	reqCancelCo, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/carryover?carry_year=2026&admin_passcode=888888", nil)
	respCancelCo, _ := http.DefaultClient.Do(reqCancelCo)
	if respCancelCo.StatusCode != 200 {
		t.Fatalf("撤销年度结转失败，状态码: %d", respCancelCo.StatusCode)
	}

	// ==================== 6. 复用品报废与管理员取消报废 ====================
	var reusableItemID int64
	_ = db.QueryRow(`SELECT id FROM items WHERE nature = 'reusable' LIMIT 1`).Scan(&reusableItemID)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = mw.WriteField("quantity", "1")
	fw, _ := mw.CreateFormFile("image_file", "desk.jpg")
	_, _ = fw.Write([]byte("img"))
	_ = mw.Close()
	_, _ = http.Post(baseURL+"/api/reusables/assets", mw.FormDataContentType(), &buf)

	var assetID int64
	_ = db.QueryRow(`SELECT id FROM reusable_assets LIMIT 1`).Scan(&assetID)

	// 报废
	var bufScrap bytes.Buffer
	mwS := multipart.NewWriter(&bufScrap)
	_ = mwS.WriteField("action", "SCRAP")
	sfw, _ := mwS.CreateFormFile("scrap_file", "scrap.pdf")
	_, _ = sfw.Write([]byte("pdf"))
	_ = mwS.Close()
	_, _ = http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwS.FormDataContentType(), &bufScrap)

	// 取消报废（错误口令拦截 + 正确口令恢复为闲置）
	var bufUnscrapBad bytes.Buffer
	mwU1 := multipart.NewWriter(&bufUnscrapBad)
	_ = mwU1.WriteField("action", "UNSCRAP")
	_ = mwU1.WriteField("admin_passcode", "wrong")
	_ = mwU1.Close()
	respU1, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwU1.FormDataContentType(), &bufUnscrapBad)
	if respU1.StatusCode != 403 {
		t.Fatalf("错误口令取消报废必须被 403 拦截")
	}

	var bufUnscrapOk bytes.Buffer
	mwU2 := multipart.NewWriter(&bufUnscrapOk)
	_ = mwU2.WriteField("action", "UNSCRAP")
	_ = mwU2.WriteField("admin_passcode", "888888")
	_ = mwU2.Close()
	respU2, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwU2.FormDataContentType(), &bufUnscrapOk)
	if respU2.StatusCode != 200 {
		t.Fatalf("管理员取消报废失败")
	}

	// ==================== 7. Excel 导出有效性验证 ====================
	for _, mode := range []string{"consumable_inventory", "consumable_ledger", "reusable_assets"} {
		respExp, err := http.Get(fmt.Sprintf("%s/api/export/excel?mode=%s&org_name=财务资产处", baseURL, mode))
		if err != nil || respExp.StatusCode != 200 {
			t.Fatalf("导出 Excel (%s) 失败", mode)
		}
		rawXlsx, _ := io.ReadAll(respExp.Body)
		_ = respExp.Body.Close()
		xlFile, err := excelize.OpenReader(bytes.NewReader(rawXlsx))
		if err != nil {
			t.Fatalf("导出的 Excel (%s) 解析失败: %v", mode, err)
		}
		_ = xlFile.Close()
	}
}
