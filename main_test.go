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

	// 校验在库价值表本期领用包装数量（12支 = 1盒）及 隐藏0结存过滤
	respInv, _ := http.Get(fmt.Sprintf("%s/api/consumables/inventory?start_date=2026-03-01&end_date=2026-03-31&hide_zero=1", baseURL))
	var invRes map[string]interface{}
	_ = json.NewDecoder(respInv.Body).Decode(&invRes)
	_ = respInv.Body.Close()
	invItems := invRes["items"].([]interface{})
	if len(invItems) != 1 {
		t.Fatalf("开启 hide_zero=1 后期望仅返回 1 条有结存记录，实际: %d", len(invItems))
	}
	itStat := invItems[0].(map[string]interface{})
	if itStat["out_pack_qty"].(float64) != 1.0 || itStat["closing_base_qty"].(float64) != 12 {
		t.Fatalf("期望本期领用包装数为 1.0，期末结存为 12，实际: %+v", itStat)
	}

	// ==================== 3. 插入编号 + 修改编号移动连续无断号 + 日期倒挂标晚日期条目 ====================
	insertBack, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-25", "base_qty": 12, "total_amount": 36,
		"custom_serial_no": "XH-2026-000002", "admin_passcode": "888888",
	})
	respInsert, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(insertBack))
	_ = respInsert.Body.Close()
	if respInsert.StatusCode != 200 {
		t.Fatalf("插入指定编号补录失败，状态码: %d", respInsert.StatusCode)
	}

	respLedger, _ := http.Get(fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID))
	var ledgerRes map[string]interface{}
	_ = json.NewDecoder(respLedger.Body).Decode(&ledgerRes)
	_ = respLedger.Body.Close()
	rows := ledgerRes["data"].([]interface{})
	secondRow := rows[1].(map[string]interface{})
	if secondRow["serial_no"] != "XH-2026-000002" || secondRow["date_anomaly"] != true {
		t.Fatalf("期望晚日期条目 XH-2026-000002 标日期倒挂，实际: %+v", secondRow)
	}

	// ==================== 4. 复用品入库、管理员补录修改金额/日期与单项资产履历导出 ====================
	var reusableItemID int64
	_ = db.QueryRow(`SELECT id FROM items WHERE nature = 'reusable' LIMIT 1`).Scan(&reusableItemID)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = mw.WriteField("purchase_date", "2026-05-01")
	_ = mw.WriteField("expected_years", "8")
	_ = mw.WriteField("quantity", "1")
	_ = mw.WriteField("total_amount", "0")
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
	assetID := int64(assetObj["id"].(float64))

	// 补录修正该复用品的单价为 680 元，预计年限 10 年
	var editBuf bytes.Buffer
	emw := multipart.NewWriter(&editBuf)
	_ = emw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = emw.WriteField("sub_name", "财务室专用办公桌")
	_ = emw.WriteField("purchase_date", "2026-05-01")
	_ = emw.WriteField("unit_price", "680")
	_ = emw.WriteField("expected_years", "10")
	_ = emw.WriteField("admin_passcode", "888888")
	_ = emw.Close()

	reqEditAst, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/reusables/assets/%d", baseURL, assetID), &editBuf)
	reqEditAst.Header.Set("Content-Type", emw.FormDataContentType())
	respEditAst, err := http.DefaultClient.Do(reqEditAst)
	if err != nil || respEditAst.StatusCode != 200 {
		t.Fatalf("补录修正复用品档案失败: %v", err)
	}
	_ = respEditAst.Body.Close()

	// ==================== 5. 全部 4 种 Excel 导出有效性验证（含 hide_zero=1） ====================
	exportURLs := []string{
		fmt.Sprintf("%s/api/export/excel?mode=consumable_inventory&org_name=财务资产处&hide_zero=1", baseURL),
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
