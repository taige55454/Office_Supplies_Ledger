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
	"strings"
	"testing"

	"github.com/xuri/excelize/v2"
)

func doJSONReq(t *testing.T, method, url, token string, payload interface{}) (*http.Response, map[string]interface{}) {
	t.Helper()
	var bodyReader io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("序列化请求失败: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, url, bodyReader)
	if err != nil {
		t.Fatalf("构建请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("X-Auth-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求 %s %s 失败: %v", method, url, err)
	}
	defer resp.Body.Close()
	var res map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&res)
	return resp, res
}

func TestFullBusiness100PercentCoverage(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "office-ledger-full-test-*")
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

	// =========================================================================
	// 模块 1：鉴权、RBAC 角色隔离与路径穿越安全
	// =========================================================================
	respBadLogin, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/auth/login", "", map[string]string{"passcode": "wrongpass"})
	if respBadLogin.StatusCode != http.StatusUnauthorized {
		t.Fatalf("错误口令应返回 401，实际: %d", respBadLogin.StatusCode)
	}

	respEntryLogin, entryLoginRes := doJSONReq(t, http.MethodPost, baseURL+"/api/auth/login", "", map[string]string{"passcode": "666666"})
	if respEntryLogin.StatusCode != http.StatusOK || entryLoginRes["role"] != "entry" {
		t.Fatalf("内网登记员登录失败: %+v", entryLoginRes)
	}
	entryToken := entryLoginRes["token"].(string)

	respAdminLogin, adminLoginRes := doJSONReq(t, http.MethodPost, baseURL+"/api/auth/login", "", map[string]string{"passcode": "888888"})
	if respAdminLogin.StatusCode != http.StatusOK || adminLoginRes["role"] != "admin" {
		t.Fatalf("管理员登录失败: %+v", adminLoginRes)
	}
	adminToken := adminLoginRes["token"].(string)

	respGuest, _ := doJSONReq(t, http.MethodGet, baseURL+"/api/items", "invalid|token|sig", nil)
	if respGuest.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无效 token 必须返回 401，实际: %d", respGuest.StatusCode)
	}

	respEntryPut, _ := doJSONReq(t, http.MethodPut, baseURL+"/api/items/1", entryToken, map[string]interface{}{"name": "非法修改"})
	if respEntryPut.StatusCode != http.StatusForbidden {
		t.Fatalf("登记员 PUT 修改必须被拦截 (403)，实际: %d", respEntryPut.StatusCode)
	}
	respEntryDel, _ := doJSONReq(t, http.MethodDelete, baseURL+"/api/items/1?admin_passcode=888888", entryToken, nil)
	if respEntryDel.StatusCode != http.StatusForbidden {
		t.Fatalf("登记员 DELETE 删除必须被拦截 (403)，实际: %d", respEntryDel.StatusCode)
	}
	respEntrySet, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/settings", entryToken, map[string]interface{}{"port": 8099})
	if respEntrySet.StatusCode != http.StatusForbidden {
		t.Fatalf("登记员修改设置必须被拦截 (403)，实际: %d", respEntrySet.StatusCode)
	}

	respTraversal, err := http.Get(baseURL + "/attachments/..%2Fconfig.json")
	if err != nil {
		t.Fatalf("路径穿越测试请求异常: %v", err)
	}
	_ = respTraversal.Body.Close()
	if respTraversal.StatusCode != http.StatusForbidden && respTraversal.StatusCode != http.StatusNotFound {
		t.Fatalf("路径穿越必须被拦截，实际状态码: %d", respTraversal.StatusCode)
	}

	respSetOk, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/settings", adminToken, map[string]interface{}{
		"port":           currentPort,
		"admin_passcode": "888888",
		"entry_passcode": "666666",
		"allow_lan":      true,
		"org_name":       "集团综合行政部",
	})
	if respSetOk.StatusCode != http.StatusOK {
		t.Fatalf("管理员保存设置失败: %d", respSetOk.StatusCode)
	}

	// =========================================================================
	// 模块 2：用品库新增、重复拦截、包装规格修改与历史台账数量守恒倒挤、权限删除
	// =========================================================================
	respNewItem, createItemRes := doJSONReq(t, http.MethodPost, baseURL+"/api/items", entryToken, map[string]interface{}{
		"category": "办公文具", "name": "全覆盖测试笔", "nature": "consumable",
		"base_unit": "支", "pack_unit": "盒", "pack_size": 12,
	})
	if respNewItem.StatusCode != http.StatusOK {
		t.Fatalf("新增用品失败: %+v", createItemRes)
	}
	itemID := int64(createItemRes["id"].(float64))

	// 2.1 新增一个无业务关联的临时用品，测试管理员口令校验及删除功能
	_, tempItemRes := doJSONReq(t, http.MethodPost, baseURL+"/api/items", adminToken, map[string]interface{}{
		"category": "办公文具", "name": "误建待删用品", "nature": "consumable",
		"base_unit": "个", "pack_unit": "盒", "pack_size": 10,
	})
	tempItemID := int64(tempItemRes["id"].(float64))

	// 错口令删除拦截
	respBadDelItem, _ := doJSONReq(t, http.MethodDelete, fmt.Sprintf("%s/api/items/%d?admin_passcode=wrong", baseURL, tempItemID), adminToken, nil)
	if respBadDelItem.StatusCode != http.StatusForbidden {
		t.Fatalf("错误口令删除用品库条目必须返回 403，实际: %d", respBadDelItem.StatusCode)
	}
	// 正确口令删除成功
	respOkDelItem, _ := doJSONReq(t, http.MethodDelete, fmt.Sprintf("%s/api/items/%d?admin_passcode=888888", baseURL, tempItemID), adminToken, nil)
	if respOkDelItem.StatusCode != http.StatusOK {
		t.Fatalf("无流水关联用品删除失败: %d", respOkDelItem.StatusCode)
	}

	// 2.2 同分类同性质同名重复拦截 (409)
	respDupItem, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/items", adminToken, map[string]interface{}{
		"category": "办公文具", "name": "全覆盖测试笔", "nature": "consumable",
		"base_unit": "支", "pack_unit": "盒", "pack_size": 12,
	})
	if respDupItem.StatusCode != http.StatusConflict {
		t.Fatalf("同名用品重复新增必须返回 409，实际: %d", respDupItem.StatusCode)
	}

	// 2.3 小数基本数量拦截 (400)
	respDec, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 3.5, "total_amount": 10,
	})
	if respDec.StatusCode != http.StatusBadRequest {
		t.Fatalf("小数基本数量必须拦截 (400)，实际: %d", respDec.StatusCode)
	}

	// =========================================================================
	// 模块 3：消耗品出入库、实时动态倒挤包装尾差、负库存拦截、有流水关联禁止删除
	// =========================================================================
	_, _ = doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 24, "total_amount": 72,
	})

	// 已有流水记录的用品，即使输入正确管理员口令也必须被引用完整性拦截 (409)
	respDelUsedItem, _ := doJSONReq(t, http.MethodDelete, fmt.Sprintf("%s/api/items/%d?admin_passcode=888888", baseURL, itemID), adminToken, nil)
	if respDelUsedItem.StatusCode != http.StatusConflict {
		t.Fatalf("已有流水关联的用品必须禁止删除 (409)，实际: %d", respDelUsedItem.StatusCode)
	}

	var packSteps []float64
	for i := 1; i <= 3; i++ {
		_, outRes := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
			"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-20", "base_qty": 4, "recipient": fmt.Sprintf("领用人%d", i),
		})
		packSteps = append(packSteps, outRes["reconciled_pack_qty"].(float64))
	}
	if packSteps[0] != 0.33 || packSteps[1] != 0.34 || packSteps[2] != 0.33 {
		t.Fatalf("期望实时倒挤包装数为 [0.33, 0.34, 0.33]，实际: %v", packSteps)
	}

	respOverOut, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-21", "base_qty": 15, "recipient": "超领人",
	})
	if respOverOut.StatusCode != http.StatusBadRequest {
		t.Fatalf("超库存领用必须被拦截 (400)，实际: %d", respOverOut.StatusCode)
	}

	respEditItem, _ := doJSONReq(t, http.MethodPut, fmt.Sprintf("%s/api/items/%d", baseURL, itemID), adminToken, map[string]interface{}{
		"category": "办公文具", "name": "全覆盖测试笔", "base_unit": "支", "pack_unit": "盒", "pack_size": 10, "admin_passcode": "888888",
	})
	if respEditItem.StatusCode != http.StatusOK {
		t.Fatalf("修改用品规格失败: %d", respEditItem.StatusCode)
	}
	_, ledgerAfterSpec := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID), adminToken, nil)
	specRows := ledgerAfterSpec["data"].([]interface{})
	if specRows[1].(map[string]interface{})["pack_qty"].(float64) != 0.4 {
		t.Fatalf("修改包装规格为10后，4支对应的折算包装数量应为 0.4，实际: %v", specRows[1].(map[string]interface{})["pack_qty"])
	}
	_, _ = doJSONReq(t, http.MethodPut, fmt.Sprintf("%s/api/items/%d", baseURL, itemID), adminToken, map[string]interface{}{
		"category": "办公文具", "name": "全覆盖测试笔", "base_unit": "支", "pack_unit": "盒", "pack_size": 12, "admin_passcode": "888888",
	})

	// =========================================================================
	// 模块 4：插入编号、双向无断号重排、日期倒挂标晚日期条目、前后数量、插号负库存回滚
	// =========================================================================
	respNeedOverride, ovRes := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-05", "base_qty": 12, "total_amount": 48,
	})
	if respNeedOverride.StatusCode != http.StatusConflict || ovRes["need_override"] != true {
		t.Fatalf("补录前期单据未带口令应返回 409 need_override，实际: %+v", ovRes)
	}

	respInsert, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", adminToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-25", "base_qty": 12, "total_amount": 48,
		"custom_serial_no": "XH-2026-000002", "admin_passcode": "888888",
	})
	if respInsert.StatusCode != http.StatusOK {
		t.Fatalf("插号补录失败: %d", respInsert.StatusCode)
	}

	respNegInsert, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", adminToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-01", "base_qty": 5, "recipient": "测试员",
		"custom_serial_no": "XH-2026-000001", "admin_passcode": "888888",
	})
	if respNegInsert.StatusCode != http.StatusBadRequest {
		t.Fatalf("插号导致中间时点负库存必须被拦截并回滚，实际: %d", respNegInsert.StatusCode)
	}

	_, ledgerRes := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID), adminToken, nil)
	rows := ledgerRes["data"].([]interface{})
	if len(rows) != 5 {
		t.Fatalf("期望 5 条流水记录，实际: %d", len(rows))
	}
	r2 := rows[1].(map[string]interface{})
	r3 := rows[2].(map[string]interface{})
	if r2["serial_no"] != "XH-2026-000002" || r2["date_anomaly"] != true {
		t.Fatalf("日期倒挂应标在较晚日期条目 XH-2026-000002 上，实际: %+v", r2)
	}
	if r3["date_anomaly"] != false {
		t.Fatalf("较早日期条目 XH-2026-000003 不应被误标，实际: %+v", r3)
	}
	if r2["before_qty"].(float64) != 24 || r2["after_qty"].(float64) != 36 {
		t.Fatalf("入库前后数量应为 24 -> 36，实际: %v -> %v", r2["before_qty"], r2["after_qty"])
	}

	fourthID := int64(rows[3].(map[string]interface{})["id"].(float64))
	respMovePut, _ := doJSONReq(t, http.MethodPut, fmt.Sprintf("%s/api/consumables/ledger/%d", baseURL, fourthID), adminToken, map[string]interface{}{
		"serial_no": "XH-2026-000002", "tx_date": "2026-03-20", "base_qty": 4,
		"recipient": "领用人2", "admin_passcode": "888888",
	})
	if respMovePut.StatusCode != http.StatusOK {
		t.Fatalf("修改编号插队失败: %d", respMovePut.StatusCode)
	}
	_, ledgerAfterMove := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID), adminToken, nil)
	movedRows := ledgerAfterMove["data"].([]interface{})
	for i, raw := range movedRows {
		expectedNo := fmt.Sprintf("XH-2026-%06d", i+1)
		gotNo := raw.(map[string]interface{})["serial_no"].(string)
		if gotNo != expectedNo {
			t.Fatalf("修改编号移动后出现断号或跳号！第 %d 条期望 %s，实际 %s", i+1, expectedNo, gotNo)
		}
	}

	// =========================================================================
	// 模块 5：在库价值盘点表时间区间恒等式、本期领用包装数、隐藏0结存、采购价格正态分布
	// =========================================================================
	_, invCutRes := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/consumables/inventory?start_date=2026-03-01&end_date=2026-03-21&hide_zero=1", baseURL), adminToken, nil)
	cutItems := invCutRes["items"].([]interface{})
	if len(cutItems) != 1 {
		t.Fatalf("开启 hide_zero=1 后应仅返回 1 条有结存品种，实际: %d", len(cutItems))
	}
	cutStat := cutItems[0].(map[string]interface{})
	if cutStat["opening_base_qty"].(float64) != 0 ||
		cutStat["in_base_qty"].(float64) != 24 ||
		cutStat["out_base_qty"].(float64) != 12 ||
		cutStat["out_pack_qty"].(float64) != 1.0 ||
		cutStat["closing_base_qty"].(float64) != 12 {
		t.Fatalf("时间区间截断恒等式或本期领用包装数异常: %+v", cutStat)
	}

	respDist, distRes := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/items/price-distribution?item_id=%d", baseURL, itemID), adminToken, nil)
	if respDist.StatusCode != http.StatusOK || distRes["has_data"] != true || distRes["min_price"].(float64) != 3.0 || distRes["max_price"].(float64) != 4.0 {
		t.Fatalf("采购价格正态分布分析结果异常: %+v", distRes)
	}

	// =========================================================================
	// 模块 6：年度结转、次年编号重置、撤销年度结转
	// =========================================================================
	respCarry, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/carryover", adminToken, map[string]interface{}{
		"carry_year": 2026, "admin_passcode": "888888",
	})
	if respCarry.StatusCode != http.StatusOK {
		t.Fatalf("执行 2026 年度结转失败: %d", respCarry.StatusCode)
	}
	_, carryListRes := doJSONReq(t, http.MethodGet, baseURL+"/api/carryover", adminToken, nil)
	if len(carryListRes["data"].([]interface{})) != 1 {
		t.Fatalf("结转记录应为 1 条")
	}
	respCancelCarry, _ := doJSONReq(t, http.MethodDelete, baseURL+"/api/carryover?carry_year=2026&admin_passcode=888888", adminToken, nil)
	if respCancelCarry.StatusCode != http.StatusOK {
		t.Fatalf("撤销年度结转失败: %d", respCancelCarry.StatusCode)
	}

	// =========================================================================
	// 模块 7：复用品（类资产）全生命周期
	// =========================================================================
	var reusableItemID int64
	_ = db.QueryRow(`SELECT id FROM items WHERE nature = 'reusable' LIMIT 1`).Scan(&reusableItemID)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = mw.WriteField("sub_name", "301室订书机")
	_ = mw.WriteField("purchase_date", "2026-05-01")
	_ = mw.WriteField("expected_years", "5")
	_ = mw.WriteField("quantity", "1")
	_ = mw.WriteField("total_amount", "0")
	fw, _ := mw.CreateFormFile("image_file", "stapler.jpg")
	_, _ = fw.Write([]byte("fake-image-bytes"))
	_ = mw.Close()

	reqAstIn, _ := http.NewRequest(http.MethodPost, baseURL+"/api/reusables/assets", &buf)
	reqAstIn.Header.Set("Content-Type", mw.FormDataContentType())
	reqAstIn.Header.Set("X-Auth-Token", entryToken)
	respAstIn, err := http.DefaultClient.Do(reqAstIn)
	if err != nil {
		t.Fatalf("复用品入库请求失败: %v", err)
	}
	_ = respAstIn.Body.Close()
	if respAstIn.StatusCode != http.StatusOK {
		t.Fatalf("复用品入库建档失败，状态码: %d", respAstIn.StatusCode)
	}

	_, assetsRes := doJSONReq(t, http.MethodGet, baseURL+"/api/reusables/assets", adminToken, nil)
	assetObj := assetsRes["data"].([]interface{})[0].(map[string]interface{})
	assetID := int64(assetObj["id"].(float64))
	if assetObj["disposal_date"] != "2031-05-01" {
		t.Fatalf("预计处置日期应为 2031-05-01，实际: %v", assetObj["disposal_date"])
	}

	var editAstBuf bytes.Buffer
	emw := multipart.NewWriter(&editAstBuf)
	_ = emw.WriteField("item_id", fmt.Sprintf("%d", reusableItemID))
	_ = emw.WriteField("sub_name", "301室重型订书机")
	_ = emw.WriteField("purchase_date", "2026-05-01")
	_ = emw.WriteField("unit_price", "128.5")
	_ = emw.WriteField("expected_years", "8")
	_ = emw.WriteField("remark", "财务补录发票金额")
	_ = emw.WriteField("admin_passcode", "888888")
	_ = emw.Close()
	reqEditAst, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("%s/api/reusables/assets/%d", baseURL, assetID), &editAstBuf)
	reqEditAst.Header.Set("Content-Type", emw.FormDataContentType())
	reqEditAst.Header.Set("X-Auth-Token", adminToken)
	respEditAst, err := http.DefaultClient.Do(reqEditAst)
	if err != nil {
		t.Fatalf("补录修正复用品请求失败: %v", err)
	}
	_ = respEditAst.Body.Close()
	if respEditAst.StatusCode != http.StatusOK {
		t.Fatalf("管理员补录修正复用品档案失败: %d", respEditAst.StatusCode)
	}

	postTransition := func(fields map[string]string, withFile bool) int {
		t.Helper()
		var b bytes.Buffer
		w := multipart.NewWriter(&b)
		for k, v := range fields {
			_ = w.WriteField(k, v)
		}
		if withFile {
			f, _ := w.CreateFormFile("scrap_file", "scrap.pdf")
			_, _ = f.Write([]byte("scrap-doc"))
		}
		_ = w.Close()
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), &b)
		if err != nil {
			t.Fatalf("构建状态流转请求失败: %v", err)
		}
		req.Header.Set("Content-Type", w.FormDataContentType())
		req.Header.Set("X-Auth-Token", adminToken)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("执行状态流转请求 (%s) 失败: %v", fields["action"], err)
		}
		defer r.Body.Close()
		return r.StatusCode
	}

	if code := postTransition(map[string]string{"action": "ASSIGN", "user": "张三"}, false); code != http.StatusOK {
		t.Fatalf("首次领用失败: %d", code)
	}
	if code := postTransition(map[string]string{"action": "ASSIGN", "user": "李四"}, false); code != http.StatusBadRequest {
		t.Fatalf("重复领用必须被拦截 (400)，实际: %d", code)
	}
	if code := postTransition(map[string]string{"action": "TRANSFER", "user": "王五"}, false); code != http.StatusOK {
		t.Fatalf("变更使用人失败: %d", code)
	}
	if code := postTransition(map[string]string{"action": "SCRAP"}, false); code != http.StatusBadRequest {
		t.Fatalf("未传报废凭证必须拦截 (400)，实际: %d", code)
	}
	if code := postTransition(map[string]string{"action": "SCRAP"}, true); code != http.StatusOK {
		t.Fatalf("上传凭证登记报废失败: %d", code)
	}
	if code := postTransition(map[string]string{"action": "ASSIGN", "user": "赵六"}, false); code != http.StatusBadRequest {
		t.Fatalf("报废后领用必须被拦截 (400)，实际: %d", code)
	}
	if code := postTransition(map[string]string{"action": "UNSCRAP", "admin_passcode": "888888"}, false); code != http.StatusOK {
		t.Fatalf("管理员取消报废失败: %d", code)
	}

	// =========================================================================
	// 模块 8：月度原始手写登记附件上传、修改、删除与审计日志验证
	// =========================================================================
	var maBuf bytes.Buffer
	mmw := multipart.NewWriter(&maBuf)
	_ = mmw.WriteField("year_month", "2026-03")
	_ = mmw.WriteField("title", "2026年3月手写领用登记表")
	_ = mmw.WriteField("remark", "共2页")
	mfw, _ := mmw.CreateFormFile("file", "handwrite_202603.pdf")
	_, _ = mfw.Write([]byte("%PDF-1.4 mock"))
	_ = mmw.Close()
	reqMaUp, _ := http.NewRequest(http.MethodPost, baseURL+"/api/monthly-attachments", &maBuf)
	reqMaUp.Header.Set("Content-Type", mmw.FormDataContentType())
	reqMaUp.Header.Set("X-Auth-Token", entryToken)
	respMaUp, err := http.DefaultClient.Do(reqMaUp)
	if err != nil {
		t.Fatalf("上传月度附件请求失败: %v", err)
	}
	var maUpRes map[string]interface{}
	_ = json.NewDecoder(respMaUp.Body).Decode(&maUpRes)
	_ = respMaUp.Body.Close()
	if respMaUp.StatusCode != http.StatusOK {
		t.Fatalf("上传月度附件失败: %d", respMaUp.StatusCode)
	}
	maID := int64(maUpRes["id"].(float64))
	relFilePath := maUpRes["file_path"].(string)

	respMaDel, _ := doJSONReq(t, http.MethodDelete, fmt.Sprintf("%s/api/monthly-attachments/%d?admin_passcode=888888", baseURL, maID), adminToken, nil)
	if respMaDel.StatusCode != http.StatusOK {
		t.Fatalf("管理员删除月度附件失败: %d", respMaDel.StatusCode)
	}
	if _, err := os.Stat(filepath.Join(attachRootDir, filepath.FromSlash(relFilePath))); !os.IsNotExist(err) {
		t.Fatalf("删除月度附件后物理文件未清理")
	}

	_, auditRes := doJSONReq(t, http.MethodGet, baseURL+"/api/audit-logs", adminToken, nil)
	if len(auditRes["data"].([]interface{})) < 5 {
		t.Fatalf("审计日志应记录全部敏感操作")
	}

	// =========================================================================
	// 模块 9：全部 4 种 Excel 导出结构、表头字段与 hide_zero 深度单元格校验
	// =========================================================================
	fetchExcel := func(u string) *excelize.File {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, u, nil)
		req.Header.Set("X-Auth-Token", adminToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("导出 Excel (%s) 失败: %v", u, err)
		}
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		xl, err := excelize.OpenReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("解析 Excel (%s) 失败: %v", u, err)
		}
		return xl
	}

	xlInv := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=consumable_inventory&hide_zero=1", baseURL))
	defer xlInv.Close()
	orgCell, _ := xlInv.GetCellValue("消耗品盘点统计表", "A2")
	unitCell, _ := xlInv.GetCellValue("消耗品盘点统计表", "P2")
	outPackHeader, _ := xlInv.GetCellValue("消耗品盘点统计表", "L3")
	row5Seq, _ := xlInv.GetCellValue("消耗品盘点统计表", "A5")
	if !strings.Contains(orgCell, "集团综合行政部") || unitCell != "金额单位：元" || outPackHeader != "本期领用(包装数量)" || row5Seq != "" {
		t.Fatalf("盘点表 Excel 表头或 hide_zero 过滤校验失败: org=%s, unit=%s, L3=%s, A5=%s", orgCell, unitCell, outPackHeader, row5Seq)
	}

	xlLed := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=consumable_ledger", baseURL))
	defer xlLed.Close()
	hBefore, _ := xlLed.GetCellValue("消耗品接续流水账", "I3")
	hQty, _ := xlLed.GetCellValue("消耗品接续流水账", "J3")
	hAfter, _ := xlLed.GetCellValue("消耗品接续流水账", "K3")
	hPack, _ := xlLed.GetCellValue("消耗品接续流水账", "L3")
	if hBefore != "入库/领用前数量" || hQty != "入库/领用数量" || hAfter != "入库/领用后数量" || hPack != "折算包装数量" {
		t.Fatalf("流水账 Excel 列头校验失败: I3=%s, J3=%s, K3=%s, L3=%s", hBefore, hQty, hAfter, hPack)
	}

	xlAst := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=reusable_assets", baseURL))
	defer xlAst.Close()
	astHeaders, _ := xlAst.GetRows("复用品类资产台账")
	for _, h := range astHeaders[2] {
		if strings.Contains(h, "实物图") {
			t.Fatalf("复用品台账 Excel 不应包含实物图列")
		}
	}

	xlLog := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=reusable_single_logs&asset_id=%d", baseURL, assetID))
	defer xlLog.Close()
	logRows, _ := xlLog.GetRows("资产全生命周期履历")
	if len(logRows) < 8 {
		t.Fatalf("单项资产履历 Excel 行数不足，期望含表头至少 8 行，实际: %d", len(logRows))
	}
}
