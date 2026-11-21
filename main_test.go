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
	_ = resp1.Body.Close()
	itemID := int64(createItemRes["id"].(float64))

	// ==================== 2. 基本数量强制整数 + 每次登记实时动态倒挤包装尾差 ====================
	decPayload, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 3.96, "total_amount": 10,
	})
	respDec, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(decPayload))
	_ = respDec.Body.Close()
	if respDec.StatusCode != 400 {
		t.Fatalf("小数基本数量必须被拦截，实际状态码: %d", respDec.StatusCode)
	}

	// 入库 24 支（2盒），总价 72 元（单价 3 元） -> XH-2026-000001
	in1, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 24, "total_amount": 72,
	})
	rIn, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(in1))
	_ = rIn.Body.Close()

	// 连续 3 次每次领用 4 支 -> XH-2026-000002, 000003, 000004
	var packSteps []float64
	for i := 1; i <= 3; i++ {
		outP, _ := json.Marshal(map[string]interface{}{
			"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-20", "base_qty": 4, "recipient": fmt.Sprintf("领用人%d", i),
		})
		respOut, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(outP))
		var outRes map[string]interface{}
		_ = json.NewDecoder(respOut.Body).Decode(&outRes)
		_ = respOut.Body.Close()
		packSteps = append(packSteps, outRes["reconciled_pack_qty"].(float64))
	}
	if packSteps[0] != 0.33 || packSteps[1] != 0.34 || packSteps[2] != 0.33 {
		t.Fatalf("期望每次登记实时倒挤包装数为 [0.33, 0.34, 0.33]，实际: %v", packSteps)
	}

	// ==================== 3. 插入编号 + 修改编号移动连续无断号 + 日期倒挂标晚日期条目 + 前后数量校验 ====================
	// 在 XH-2026-000002 位置插入一笔 2026-03-25 的单据（日期故意晚于 000003 的 2026-03-20，触发日期倒挂标在 25日 这一条上）
	insertBack, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-25", "base_qty": 12, "total_amount": 36,
		"custom_serial_no": "XH-2026-000002", "admin_passcode": "888888",
	})
	respInsert, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(insertBack))
	_ = respInsert.Body.Close()
	if respInsert.StatusCode != 200 {
		t.Fatalf("插入指定编号补录失败，状态码: %d", respInsert.StatusCode)
	}

	// 查询流水，检查第2条（2026-03-25）被标日期倒挂，第3条（2026-03-20）不被误标，且 before_qty / after_qty 准确
	respLedger, _ := http.Get(fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID))
	var ledgerRes map[string]interface{}
	_ = json.NewDecoder(respLedger.Body).Decode(&ledgerRes)
	_ = respLedger.Body.Close()
	rows := ledgerRes["data"].([]interface{})
	if len(rows) != 5 {
		t.Fatalf("期望 5 条流水记录，实际: %d", len(rows))
	}
	secondRow := rows[1].(map[string]interface{})
	thirdRow := rows[2].(map[string]interface{})
	if secondRow["serial_no"] != "XH-2026-000002" || secondRow["date_anomaly"] != true {
		t.Fatalf("期望晚日期条目 XH-2026-000002 (03-25) 标日期倒挂，实际: %+v", secondRow)
	}
	if thirdRow["date_anomaly"] != false {
		t.Fatalf("期望早日期条目 XH-2026-000003 (03-20) 不应被误标日期倒挂，实际: %+v", thirdRow)
	}
	if secondRow["before_qty"].(float64) != 24 || secondRow["after_qty"].(float64) != 36 {
		t.Fatalf("期望入库前后数量为 24 -> 36，实际: %v -> %v", secondRow["before_qty"], secondRow["after_qty"])
	}

	// 测试修改编号插队：把第 4 条（XH-2026-000004）修改编号为 XH-2026-000002，验证 1..5 绝对连续无断号！
	fourthID := int64(rows[3].(map[string]interface{})["id"].(float64))
	editPayload, _ := json.Marshal(map[string]interface{}{
		"serial_no": "XH-2026-000002", "tx_date": "2026-03-20", "base_qty": 4,
		"recipient": "领用人2", "admin_passcode": "888888",
	})
	reqPut, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/consumables/ledger/%d", baseURL, fourthID), bytes.NewReader(editPayload))
	reqPut.Header.Set("Content-Type", "application/json")
	respPut, _ := http.DefaultClient.Do(reqPut)
	_ = respPut.Body.Close()
	if respPut.StatusCode != 200 {
		t.Fatalf("修改编号插队失败，状态码: %d", respPut.StatusCode)
	}

	respLedger2, _ := http.Get(fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID))
	var ledgerRes2 map[string]interface{}
	_ = json.NewDecoder(respLedger2.Body).Decode(&ledgerRes2)
	_ = respLedger2.Body.Close()
	rows2 := ledgerRes2["data"].([]interface{})
	for i, raw := range rows2 {
		expectedSerial := fmt.Sprintf("XH-2026-%06d", i+1)
		gotSerial := raw.(map[string]interface{})["serial_no"].(string)
		if gotSerial != expectedSerial {
			t.Fatalf("修改插队后出现断号或错号！第 %d 条期望 %s，实际 %s", i+1, expectedSerial, gotSerial)
		}
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
	rAst, _ := http.Post(baseURL+"/api/reusables/assets", mw.FormDataContentType(), &buf)
	_ = rAst.Body.Close()

	respAssets, _ := http.Get(baseURL + "/api/reusables/assets")
	var assetsRes map[string]interface{}
	_ = json.NewDecoder(respAssets.Body).Decode(&assetsRes)
	_ = respAssets.Body.Close()
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
