package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
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

	// ==================== 2. 基本数量强制整数 + 每次登记实时动态倒挤包装尾差 ====================
	decPayload, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 3.96, "total_amount": 10,
	})
	respDec, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(decPayload))
	if respDec.StatusCode != 400 {
		t.Fatalf("小数基本数量必须被拦截，实际状态码: %d", respDec.StatusCode)
	}

	// 入库 24 支（2盒），总价 72 元（单价 3 元） -> XH-2026-000001
	in1, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 24, "total_amount": 72,
	})
	_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(in1))

	// 连续 3 次每次领用 4 支 -> XH-2026-000002, 000003, 000004
	var packSteps []float64
	for i := 1; i <= 3; i++ {
		outP, _ := json.Marshal(map[string]interface{}{
			"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-20", "base_qty": 4, "recipient": fmt.Sprintf("领用人%d", i),
		})
		respOut, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(outP))
		var outRes map[string]interface{}
		_ = json.NewDecoder(respOut.Body).Decode(&outRes)
		packSteps = append(packSteps, outRes["reconciled_pack_qty"].(float64))
	}
	if packSteps[0] != 0.33 || packSteps[1] != 0.34 || packSteps[2] != 0.33 {
		t.Fatalf("期望每次登记实时倒挤包装数为 [0.33, 0.34, 0.33]，实际: %v", packSteps)
	}

	// 验证在库价值盘点表全部新列字段与本期入库包装数量
	respInv, _ := http.Get(fmt.Sprintf("%s/api/consumables/inventory?start_date=2026-03-01&end_date=2026-03-31&item_id=%d", baseURL, itemID))
	var invRes map[string]interface{}
	_ = json.NewDecoder(respInv.Body).Decode(&invRes)
	itStat := invRes["items"].([]interface{})[0].(map[string]interface{})
	if itStat["in_pack_qty"].(float64) != 2.0 || itStat["closing_base_qty"].(float64) != 12 || itStat["closing_value"].(float64) != 36.0 {
		t.Fatalf("在库价值盘点表字段核对异常: %+v", itStat)
	}

	// ==================== 3. 插入指定编号顺延 + 日期倒挂预警检测 ====================
	// 在 XH-2026-000002 位置插入一笔 2026-03-05 的历史单据（日期故意早于 000001 的 2026-03-10，触发日期倒挂预警）
	insertBack, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-05", "base_qty": 12, "total_amount": 36,
		"custom_serial_no": "XH-2026-000002", "admin_passcode": "888888",
	})
	respInsert, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(insertBack))
	if respInsert.StatusCode != 200 {
		t.Fatalf("插入指定编号补录失败，状态码: %d", respInsert.StatusCode)
	}

	// 验证流水账按编号排序，且 000002 检测到日期倒挂，原 000002~000004 已顺延为 000003~000005
	respLedger, _ := http.Get(fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID))
	var ledgerRes map[string]interface{}
	_ = json.NewDecoder(respLedger.Body).Decode(&ledgerRes)
	rows := ledgerRes["data"].([]interface{})
	if len(rows) != 5 {
		t.Fatalf("期望 5 条流水记录，实际: %d", len(rows))
	}
	secondRow := rows[1].(map[string]interface{})
	fifthRow := rows[4].(map[string]interface{})
	if secondRow["serial_no"] != "XH-2026-000002" || secondRow["date_anomaly"] != true {
		t.Fatalf("期望第2条为插入的 XH-2026-000002 且触发日期倒挂提示，实际: %+v", secondRow)
	}
	if fifthRow["serial_no"] != "XH-2026-000005" {
		t.Fatalf("期望原末条顺延为 XH-2026-000005，实际: %+v", fifthRow)
	}

	// ==================== 4. 复用品入库（预计处置日期推算）与单项资产履历导出 ====================
	var reusableItemID int64
	_ = db.QueryRow(`SELECT id FROM items WHERE nature = 'reusable' LIMIT 1`).Scan(&reusableItemID)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = mw.WriteField("purchase_date", "2026-05-01")
	_ = mw.WriteField("expected_years", "8")
	_ = mw.WriteField("quantity", "1")
	_ = mw.WriteField("total_amount", "500")
	fw, _ := mw.CreateFormFile("image_file", "desk.jpg")
	_, _ = fw.Write([]byte("img"))
	_ = mw.Close()
	_, _ = http.Post(baseURL+"/api/reusables/assets", mw.FormDataContentType(), &buf)

	respAssets, _ := http.Get(baseURL + "/api/reusables/assets")
	var assetsRes map[string]interface{}
	_ = json.NewDecoder(respAssets.Body).Decode(&assetsRes)
	assetObj := assetsRes["data"].([]interface{})[0].(map[string]interface{})
	if assetObj["disposal_date"] != "2034-05-01" {
		t.Fatalf("期望预计处置日期为 2034-05-01，实际: %v", assetObj["disposal_date"])
	}
	assetID := int64(assetObj["id"].(float64))

	// ==================== 5. 全部 4 种 Excel 导出有效性验证 ====================
	exportURLs := []string{
		fmt.Sprintf("%s/api/export/excel?mode=consumable_inventory&org_name=财务资产处", baseURL),
		fmt.Sprintf("%s/api/export/excel?mode=consumable_ledger&org_name=财务资产处", baseURL),
		fmt.Sprintf("%s/api/export/excel?mode=reusable_assets&org_name=财务资产处", baseURL),
		fmt.Sprintf("%s/api/export/excel?mode=reusable_single_logs&asset_id=%d&org_name=财务资产处", baseURL, assetID),
	}
	for _, u := range exportURLs {
		respExp, err := http.Get(u)
		if err != nil || respExp.StatusCode != 200 {
			t.Fatalf("导出 Excel (%s) 失败", u)
		}
		rawXlsx, _ := io.ReadAll(respExp.Body)
		_ = respExp.Body.Close()
		xlFile, err := excelize.OpenReader(bytes.NewReader(rawXlsx))
		if err != nil {
			t.Fatalf("导出的 Excel (%s) 解析失败: %v", u, err)
		}
		_ = xlFile.Close()
	}
}
