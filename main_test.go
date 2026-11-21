package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"testing"
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

	// 1. 验证新增用品与同分类同性质重复拦截
	body1, _ := json.Marshal(map[string]interface{}{
		"category": "办公文具", "name": "测试荧光笔", "nature": "consumable",
		"base_unit": "支", "pack_unit": "盒", "pack_size": 10,
	})
	resp1, err := http.Post(baseURL+"/api/items", "application/json", bytes.NewReader(body1))
	if err != nil || resp1.StatusCode != 200 {
		t.Fatalf("新增用品失败: %v", err)
	}
	var createItemRes map[string]interface{}
	_ = json.NewDecoder(resp1.Body).Decode(&createItemRes)
	itemID := int64(createItemRes["id"].(float64))

	// 重复提交相同分类+性质+品名，必须返回 409 冲突
	respDup, _ := http.Post(baseURL+"/api/items", "application/json", bytes.NewReader(body1))
	if respDup.StatusCode != 409 {
		t.Fatalf("期望重复品名被 409 拦截，实际状态码: %d", respDup.StatusCode)
	}

	// 2. 验证消耗品入库、单价区间计算、领用、领用人10条记忆库
	inPayload, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10",
		"base_qty": 50, "total_amount": 100, "remark": "首批采购",
	})
	respIn, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(inPayload))
	var inRes map[string]interface{}
	_ = json.NewDecoder(respIn.Body).Decode(&inRes)
	if inRes["serial_no"] != "XH-2026-000001" {
		t.Fatalf("期望流水号 XH-2026-000001，实际: %v", inRes["serial_no"])
	}

	for i := 1; i <= 12; i++ {
		outPayload, _ := json.Marshal(map[string]interface{}{
			"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-15",
			"base_qty": 2, "recipient": fmt.Sprintf("员工%d", i),
		})
		_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(outPayload))
	}
	// 领用人记忆库必须严格不超过 10 条
	var recipCnt int
	_ = db.QueryRow(`SELECT COUNT(1) FROM recipients`).Scan(&recipCnt)
	if recipCnt != 10 {
		t.Fatalf("领用人记忆库应保留最新 10 条，实际: %d", recipCnt)
	}

	// 3. 验证修改用品包装规格后，历史流水以基本数量为准重算包装数
	modPayload, _ := json.Marshal(map[string]interface{}{
		"category": "办公文具", "name": "测试荧光笔", "base_unit": "支",
		"pack_unit": "箱", "pack_size": 25, "admin_passcode": "888888",
	})
	reqMod, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/items/%d", baseURL, itemID), bytes.NewReader(modPayload))
	respMod, _ := http.DefaultClient.Do(reqMod)
	if respMod.StatusCode != 200 {
		t.Fatalf("修改用品信息失败")
	}
	var recalcPackQty float64
	_ = db.QueryRow(`SELECT pack_qty FROM consumable_ledger WHERE serial_no = 'XH-2026-000001'`).Scan(&recalcPackQty)
	if recalcPackQty != 2.0 { // 50 / 25 = 2
		t.Fatalf("期望重算后包装数为 2，实际: %v", recalcPackQty)
	}

	// 4. 验证会计期初/期末盘点统计（2026-03-12 至 2026-03-31 期初应为 50，领用 24，期末为 26）
	respInv, _ := http.Get(fmt.Sprintf("%s/api/consumables/inventory?start_date=2026-03-12&end_date=2026-03-31&item_id=%d", baseURL, itemID))
	var invRes map[string]interface{}
	_ = json.NewDecoder(respInv.Body).Decode(&invRes)
	invItems := invRes["items"].([]interface{})
	firstItem := invItems[0].(map[string]interface{})
	if firstItem["opening_base_qty"].(float64) != 50 || firstItem["closing_base_qty"].(float64) != 26 {
		t.Fatalf("期初/期末计算异常: opening=%v, closing=%v", firstItem["opening_base_qty"], firstItem["closing_base_qty"])
	}

	// 5. 验证年度结转后，次年流水号重新从 XH-2027-000001 起编
	coPayload, _ := json.Marshal(map[string]interface{}{"carry_year": 2026, "admin_passcode": "888888"})
	_, _ = http.Post(baseURL+"/api/carryover", "application/json", bytes.NewReader(coPayload))

	in2027, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2027-01-05", "base_qty": 10, "total_amount": 25,
	})
	resp2027, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(in2027))
	var res2027 map[string]interface{}
	_ = json.NewDecoder(resp2027.Body).Decode(&res2027)
	if res2027["serial_no"] != "XH-2027-000001" {
		t.Fatalf("年度结转后次年流水号应重置为 XH-2027-000001，实际: %v", res2027["serial_no"])
	}

	// 6. 验证复用品建档（一物一码）及防重复领用拦截
	var reusableItemID int64
	_ = db.QueryRow(`SELECT id FROM items WHERE nature = 'reusable' LIMIT 1`).Scan(&reusableItemID)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = mw.WriteField("sub_name", "总经理室靠窗椅")
	_ = mw.WriteField("quantity", "1")
	_ = mw.WriteField("total_amount", "850")
	fw, _ := mw.CreateFormFile("image_file", "chair.jpg")
	_, _ = fw.Write([]byte("fake-image-content"))
	_ = mw.Close()

	respAsset, _ := http.Post(baseURL+"/api/reusables/assets", mw.FormDataContentType(), &buf)
	if respAsset.StatusCode != 200 {
		t.Fatalf("复用品建档失败")
	}
	var assetID int64
	_ = db.QueryRow(`SELECT id FROM reusable_assets LIMIT 1`).Scan(&assetID)

	// 首次领用成功
	var bufAssign bytes.Buffer
	mwA := multipart.NewWriter(&bufAssign)
	_ = mwA.WriteField("action", "ASSIGN")
	_ = mwA.WriteField("user", "王总")
	_ = mwA.Close()
	respA1, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwA.FormDataContentType(), &bufAssign)
	if respA1.StatusCode != 200 {
		t.Fatalf("复用品首次领用失败")
	}

	// 再次直接领用必须被拦截
	var bufAssign2 bytes.Buffer
	mwA2 := multipart.NewWriter(&bufAssign2)
	_ = mwA2.WriteField("action", "ASSIGN")
	_ = mwA2.WriteField("user", "李经理")
	_ = mwA2.Close()
	respA2, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwA2.FormDataContentType(), &bufAssign2)
	if respA2.StatusCode != 400 {
		t.Fatalf("复用品在用状态下重复领用必须被拦截")
	}

	// 7. 验证内网登记员权限隔离（禁止修改/删除）
	entryToken := signRoleToken("entry")
	reqForbidden, _ := http.NewRequest(http.MethodDelete, baseURL+"/api/monthly-attachments/1?admin_passcode=888888", nil)
	reqForbidden.Header.Set("X-Auth-Token", entryToken)
	respForb, _ := http.DefaultClient.Do(reqForbidden)
	if respForb.StatusCode != 403 {
		t.Fatalf("内网登记员执行删除操作应返回 403，实际: %d", respForb.StatusCode)
	}
}
