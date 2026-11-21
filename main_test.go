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

	// ==================== 1. 基础字典新增与同分类同性质重复拦截 ====================
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

	respDup, _ := http.Post(baseURL+"/api/items", "application/json", bytes.NewReader(body1))
	if respDup.StatusCode != 409 {
		t.Fatalf("期望重复品名被 409 拦截，实际状态码: %d", respDup.StatusCode)
	}

	// ==================== 2. 消耗品多批次入库、单价区间计算、超库存拦截与领用人10条记忆库 ====================
	inPayload1, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10",
		"base_qty": 30, "total_amount": 60, "remark": "首批单价2元",
	})
	respIn1, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(inPayload1))
	var inRes1 map[string]interface{}
	_ = json.NewDecoder(respIn1.Body).Decode(&inRes1)
	if inRes1["serial_no"] != "XH-2026-000001" {
		t.Fatalf("期望流水号 XH-2026-000001，实际: %v", inRes1["serial_no"])
	}

	inPayload2, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-11",
		"base_qty": 20, "total_amount": 70, "remark": "二批单价3.5元",
	})
	_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(inPayload2))

	// 验证单价区间自动统计为 2.00 ~ 3.50
	var minP, maxP float64
	_ = db.QueryRow(`SELECT min_price, max_price FROM items WHERE id = ?`, itemID).Scan(&minP, &maxP)
	if minP != 2.0 || maxP != 3.5 {
		t.Fatalf("期望采购单价区间 2.00~3.50，实际: %.2f~%.2f", minP, maxP)
	}

	// 验证超库存领用必须被拦截（当前库存 50，尝试领用 51）
	overOutPayload, _ := json.Marshal(map[string]interface{}{
		"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-12",
		"base_qty": 51, "recipient": "超领测试员",
	})
	respOver, _ := http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(overOutPayload))
	if respOver.StatusCode != 400 {
		t.Fatalf("超库存领用必须被 400 拦截，实际: %d", respOver.StatusCode)
	}

	// 连续 12 次领用，验证领用人记忆库严格保留最新 10 条
	for i := 1; i <= 12; i++ {
		outPayload, _ := json.Marshal(map[string]interface{}{
			"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-15",
			"base_qty": 2, "recipient": fmt.Sprintf("员工%d", i),
		})
		_, _ = http.Post(baseURL+"/api/consumables/ledger", "application/json", bytes.NewReader(outPayload))
	}
	var recipCnt int
	_ = db.QueryRow(`SELECT COUNT(1) FROM recipients`).Scan(&recipCnt)
	if recipCnt != 10 {
		t.Fatalf("领用人记忆库应保留最新 10 条，实际: %d", recipCnt)
	}

	// ==================== 3. 修改用品包装规格后，历史流水以基本数量为准重算包装数 ====================
	modPayload, _ := json.Marshal(map[string]interface{}{
		"category": "办公文具", "name": "测试荧光笔", "base_unit": "支",
		"pack_unit": "箱", "pack_size": 15, "admin_passcode": "888888",
	})
	reqMod, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/items/%d", baseURL, itemID), bytes.NewReader(modPayload))
	respMod, _ := http.DefaultClient.Do(reqMod)
	if respMod.StatusCode != 200 {
		t.Fatalf("修改用品信息失败")
	}
	var recalcPackQty float64
	_ = db.QueryRow(`SELECT pack_qty FROM consumable_ledger WHERE serial_no = 'XH-2026-000001'`).Scan(&recalcPackQty)
	if recalcPackQty != 2.0 { // 30 / 15 = 2
		t.Fatalf("期望重算后包装数为 2，实际: %v", recalcPackQty)
	}

	// ==================== 4. 流水笔误修正（错误口令拦截 + 正确口令修正并写审计日志） ====================
	var firstLedgerID int64
	_ = db.QueryRow(`SELECT id FROM consumable_ledger WHERE serial_no = 'XH-2026-000001'`).Scan(&firstLedgerID)

	badEditPayload, _ := json.Marshal(map[string]interface{}{
		"tx_date": "2026-03-10", "base_qty": 30, "total_amount": 60, "admin_passcode": "wrong_pass",
	})
	reqBadEdit, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/consumables/ledger/%d", baseURL, firstLedgerID), bytes.NewReader(badEditPayload))
	respBadEdit, _ := http.DefaultClient.Do(reqBadEdit)
	if respBadEdit.StatusCode != 403 {
		t.Fatalf("错误管理员口令修正流水必须返回 403，实际: %d", respBadEdit.StatusCode)
	}

	goodEditPayload, _ := json.Marshal(map[string]interface{}{
		"tx_date": "2026-03-10", "base_qty": 30, "total_amount": 60, "remark": "已核对发票", "admin_passcode": "888888",
	})
	reqGoodEdit, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/consumables/ledger/%d", baseURL, firstLedgerID), bytes.NewReader(goodEditPayload))
	respGoodEdit, _ := http.DefaultClient.Do(reqGoodEdit)
	if respGoodEdit.StatusCode != 200 {
		t.Fatalf("管理员修正流水失败")
	}

	// ==================== 5. 会计期初/期末盘点统计 ====================
	// 2026-03-12 之前入库 30+20=50；03-12~03-31 领用 12*2=24，期末应为 26
	respInv, _ := http.Get(fmt.Sprintf("%s/api/consumables/inventory?start_date=2026-03-12&end_date=2026-03-31&item_id=%d", baseURL, itemID))
	var invRes map[string]interface{}
	_ = json.NewDecoder(respInv.Body).Decode(&invRes)
	invItems := invRes["items"].([]interface{})
	firstItem := invItems[0].(map[string]interface{})
	if firstItem["opening_base_qty"].(float64) != 50 || firstItem["closing_base_qty"].(float64) != 26 {
		t.Fatalf("期初/期末计算异常: opening=%v, closing=%v", firstItem["opening_base_qty"], firstItem["closing_base_qty"])
	}

	// ==================== 6. 年度结转与次年流水编号重置 ====================
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

	// ==================== 7. 复用品建档（强制传图）及全生命周期闭环（领用->防重领->变更使用人->报废->锁定） ====================
	var reusableItemID int64
	_ = db.QueryRow(`SELECT id FROM items WHERE nature = 'reusable' LIMIT 1`).Scan(&reusableItemID)

	// 7.1 未上传图片建档必须被拦截
	var bufNoImg bytes.Buffer
	mwNoImg := multipart.NewWriter(&bufNoImg)
	_ = mwNoImg.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = mwNoImg.WriteField("quantity", "1")
	_ = mwNoImg.Close()
	respNoImg, _ := http.Post(baseURL+"/api/reusables/assets", mwNoImg.FormDataContentType(), &bufNoImg)
	if respNoImg.StatusCode != 400 {
		t.Fatalf("复用品入库未上传图片必须被 400 拦截，实际: %d", respNoImg.StatusCode)
	}

	// 7.2 正常上传图片建档
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

	// 7.3 首次领用成功
	var bufAssign bytes.Buffer
	mwA := multipart.NewWriter(&bufAssign)
	_ = mwA.WriteField("action", "ASSIGN")
	_ = mwA.WriteField("user", "王总")
	_ = mwA.Close()
	respA1, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwA.FormDataContentType(), &bufAssign)
	if respA1.StatusCode != 200 {
		t.Fatalf("复用品首次领用失败")
	}

	// 7.4 重复直接领用必须被拦截
	var bufAssign2 bytes.Buffer
	mwA2 := multipart.NewWriter(&bufAssign2)
	_ = mwA2.WriteField("action", "ASSIGN")
	_ = mwA2.WriteField("user", "李经理")
	_ = mwA2.Close()
	respA2, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwA2.FormDataContentType(), &bufAssign2)
	if respA2.StatusCode != 400 {
		t.Fatalf("复用品在用状态下重复领用必须被拦截")
	}

	// 7.5 变更使用人 (TRANSFER) 必须成功
	var bufTransfer bytes.Buffer
	mwT := multipart.NewWriter(&bufTransfer)
	_ = mwT.WriteField("action", "TRANSFER")
	_ = mwT.WriteField("user", "李经理")
	_ = mwT.Close()
	respT, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwT.FormDataContentType(), &bufTransfer)
	if respT.StatusCode != 200 {
		t.Fatalf("复用品变更使用人失败")
	}

	// 7.6 报废未上传凭证必须拦截，上传凭证后成功报废，且报废后不可再流转
	var bufScrapNoFile bytes.Buffer
	mwS1 := multipart.NewWriter(&bufScrapNoFile)
	_ = mwS1.WriteField("action", "SCRAP")
	_ = mwS1.Close()
	respS1, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwS1.FormDataContentType(), &bufScrapNoFile)
	if respS1.StatusCode != 400 {
		t.Fatalf("复用品报废未上传凭证必须被 400 拦截")
	}

	var bufScrap bytes.Buffer
	mwS2 := multipart.NewWriter(&bufScrap)
	_ = mwS2.WriteField("action", "SCRAP")
	_ = mwS2.WriteField("remark", "椅腿断裂无法修复")
	sfw, _ := mwS2.CreateFormFile("scrap_file", "scrap_approval.pdf")
	_, _ = sfw.Write([]byte("fake-pdf-content"))
	_ = mwS2.Close()
	respS2, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwS2.FormDataContentType(), &bufScrap)
	if respS2.StatusCode != 200 {
		t.Fatalf("复用品上传凭证报废失败")
	}

	// 报废后再次尝试变更使用人必须被锁定拦截
	var bufAfterScrap bytes.Buffer
	mwAS := multipart.NewWriter(&bufAfterScrap)
	_ = mwAS.WriteField("action", "TRANSFER")
	_ = mwAS.WriteField("user", "赵六")
	_ = mwAS.Close()
	respAS, _ := http.Post(fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), mwAS.FormDataContentType(), &bufAfterScrap)
	if respAS.StatusCode != 400 {
		t.Fatalf("已报废资产应锁定禁止流转")
	}

	// ==================== 8. 月度手写登记附件上传、权限隔离与管理员验证删除 ====================
	var bufMonth bytes.Buffer
	mwM := multipart.NewWriter(&bufMonth)
	_ = mwM.WriteField("year_month", "2026-03")
	_ = mwM.WriteField("title", "2026年3月手写领用签字表")
	mfw, _ := mwM.CreateFormFile("file", "202603_sign.jpg")
	_, _ = mfw.Write([]byte("fake-scan-image"))
	_ = mwM.Close()

	respM, _ := http.Post(baseURL+"/api/monthly-attachments", mwM.FormDataContentType(), &bufMonth)
	if respM.StatusCode != 200 {
		t.Fatalf("上传月度手写附件失败")
	}
	var mRes map[string]interface{}
	_ = json.NewDecoder(respM.Body).Decode(&mRes)
	attID := int64(mRes["id"].(float64))
	relFilePath := mRes["file_path"].(string)

	// 8.1 内网登记员禁止删除（403）
	entryToken := signRoleToken("entry")
	reqForbidden, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/monthly-attachments/%d?admin_passcode=888888", baseURL, attID), nil)
	reqForbidden.Header.Set("X-Auth-Token", entryToken)
	respForb, _ := http.DefaultClient.Do(reqForbidden)
	if respForb.StatusCode != 403 {
		t.Fatalf("内网登记员执行删除操作应返回 403，实际: %d", respForb.StatusCode)
	}

	// 8.2 管理员口令正确删除后，物理文件同步清理
	reqDelOk, _ := http.NewRequest(http.MethodDelete, fmt.Sprintf("%s/api/monthly-attachments/%d?admin_passcode=888888", baseURL, attID), nil)
	respDelOk, _ := http.DefaultClient.Do(reqDelOk)
	if respDelOk.StatusCode != 200 {
		t.Fatalf("管理员删除月度附件失败")
	}
	if _, err := os.Stat(filepath.Join(attachRootDir, filepath.FromSlash(relFilePath))); !os.IsNotExist(err) {
		t.Fatalf("删除月度附件后磁盘文件未被清理")
	}

	// ==================== 9. 校验 3 种模式 Excel 导出文件合法性 ====================
	for _, mode := range []string{"consumable_inventory", "consumable_ledger", "reusable_assets"} {
		respExp, err := http.Get(fmt.Sprintf("%s/api/export/excel?mode=%s&start_date=2026-01-01&end_date=2027-12-31", baseURL, mode))
		if err != nil || respExp.StatusCode != 200 {
			t.Fatalf("导出 Excel (%s) 请求失败", mode)
		}
		rawXlsx, _ := io.ReadAll(respExp.Body)
		_ = respExp.Body.Close()
		xlFile, err := excelize.OpenReader(bytes.NewReader(rawXlsx))
		if err != nil {
			t.Fatalf("导出的 Excel (%s) 无法被 excelize 解析: %v", mode, err)
		}
		_ = xlFile.Close()
	}

	// ==================== 10. 校验审计日志留存 ====================
	var auditCnt int
	_ = db.QueryRow(`SELECT COUNT(1) FROM audit_logs`).Scan(&auditCnt)
	if auditCnt < 4 {
		t.Fatalf("期望至少留存 4 条敏感操作审计日志，实际: %d", auditCnt)
	}
}
