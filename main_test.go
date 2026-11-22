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
	// 模块 1：鉴权、密码有效时限配置、RBAC 角色隔离与路径穿越安全
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
		"port":               currentPort,
		"admin_passcode":     "888888",
		"entry_passcode":     "666666",
		"allow_lan":          true,
		"org_name":           "集团综合行政部",
		"token_expire_hours": 6.0,
	})
	if respSetOk.StatusCode != http.StatusOK {
		t.Fatalf("管理员保存设置失败: %d", respSetOk.StatusCode)
	}

	// =========================================================================
	// 模块 2：用品库新增、重复拦截、包装规格修改与历史台账数量守恒倒挤
	// =========================================================================
	respNewItem, createItemRes := doJSONReq(t, http.MethodPost, baseURL+"/api/items", entryToken, map[string]interface{}{
		"category": "办公文具", "name": "全覆盖测试笔", "nature": "consumable",
		"base_unit": "支", "pack_unit": "盒", "pack_size": 12,
	})
	if respNewItem.StatusCode != http.StatusOK {
		t.Fatalf("新增用品失败: %+v", createItemRes)
	}
	itemID := int64(createItemRes["id"].(float64))
	if !strings.HasPrefix(createItemRes["item_code"].(string), "YP-") {
		t.Fatalf("新增用品未生成专有编码: %+v", createItemRes)
	}

	respDupItem, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/items", adminToken, map[string]interface{}{
		"category": "办公文具", "name": "全覆盖测试笔", "nature": "consumable",
		"base_unit": "支", "pack_unit": "盒", "pack_size": 12,
	})
	if respDupItem.StatusCode != http.StatusConflict {
		t.Fatalf("同名用品重复新增必须返回 409，实际: %d", respDupItem.StatusCode)
	}

	respDec, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 3.5, "total_amount": 10,
	})
	if respDec.StatusCode != http.StatusBadRequest {
		t.Fatalf("小数基本数量必须拦截 (400)，实际: %d", respDec.StatusCode)
	}

	// =========================================================================
	// 模块 3：消耗品出入库、实时动态倒挤包装尾差、负库存拦截
	// =========================================================================
	_, _ = doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", entryToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-10", "base_qty": 24, "total_amount": 72,
	})

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
	// 模块 4：插入编号、双向无断号重排、日期倒挂标晚日期条目、插号负库存回滚
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

	// 新增一笔价格为 0 的划拨入库（验证其不拉低价格分布与合理区间）
	_, _ = doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", adminToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "IN", "tx_date": "2026-03-26", "base_qty": 10, "total_amount": 0,
		"remark": "兄弟部门划拨无价物资",
	})

	respNegInsert, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/consumables/ledger", adminToken, map[string]interface{}{
		"item_id": itemID, "tx_type": "OUT", "tx_date": "2026-03-01", "base_qty": 5, "recipient": "测试员",
		"custom_serial_no": "XH-2026-000001", "admin_passcode": "888888",
	})
	if respNegInsert.StatusCode != http.StatusBadRequest {
		t.Fatalf("插号导致中间时点负库存必须被拦截并回滚，实际: %d", respNegInsert.StatusCode)
	}

	_, ledgerRes := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/consumables/ledger?item_id=%d", baseURL, itemID), adminToken, nil)
	rows := ledgerRes["data"].([]interface{})
	if len(rows) != 6 {
		t.Fatalf("期望 6 条流水记录，实际: %d", len(rows))
	}
	r2 := rows[1].(map[string]interface{})
	r3 := rows[2].(map[string]interface{})
	if r2["serial_no"] != "XH-2026-000002" || r2["date_anomaly"] != true {
		t.Fatalf("日期倒挂应标在较晚日期条目 XH-2026-000002 上，实际: %+v", r2)
	}
	if r3["date_anomaly"] != false {
		t.Fatalf("较早日期条目 XH-2026-000003 不应被误标，实际: %+v", r3)
	}

	// =========================================================================
	// 模块 5：采购价格正态分布（必须自动排除价格为0的划拨入库记录）
	// =========================================================================
	respDist, distRes := doJSONReq(t, http.MethodGet, fmt.Sprintf("%s/api/items/price-distribution?item_id=%d", baseURL, itemID), adminToken, nil)
	if respDist.StatusCode != http.StatusOK || distRes["has_data"] != true ||
		distRes["min_price"].(float64) != 3.0 || distRes["max_price"].(float64) != 4.0 ||
		int(distRes["batch_total"].(float64)) != 2 {
		t.Fatalf("采购价格分布应忽略0元划拨记录，期望 batch_total=2, min=3.0, max=4.0，实际: %+v", distRes)
	}

	// =========================================================================
	// 模块 6：年度结转与撤销年度结转
	// =========================================================================
	respCarry, _ := doJSONReq(t, http.MethodPost, baseURL+"/api/carryover", adminToken, map[string]interface{}{
		"carry_year": 2026, "admin_passcode": "888888",
	})
	if respCarry.StatusCode != http.StatusOK {
		t.Fatalf("执行 2026 年度结转失败: %d", respCarry.StatusCode)
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
		req, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/api/reusables/assets/%d/transition", baseURL, assetID), &b)
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
	if code := postTransition(map[string]string{"action": "SCRAP"}, true); code != http.StatusOK {
		t.Fatalf("上传凭证登记报废失败: %d", code)
	}
	if code := postTransition(map[string]string{"action": "UNSCRAP", "admin_passcode": "888888"}, false); code != http.StatusOK {
		t.Fatalf("管理员取消报废失败: %d", code)
	}

	// =========================================================================
	// 模块 8：审计日志 IP 校验与全部 5 种 Excel 导出（含审计日志导出）
	// =========================================================================
	_, auditRes := doJSONReq(t, http.MethodGet, baseURL+"/api/audit-logs", adminToken, nil)
	auditList := auditRes["data"].([]interface{})
	if len(auditList) < 4 {
		t.Fatalf("审计日志条数不足")
	}
	firstAudit := auditList[0].(map[string]interface{})
	if !strings.Contains(firstAudit["client_ip"].(string), "127.0.0.1") {
		t.Fatalf("审计日志应包含操作来源 IP，实际: %+v", firstAudit)
	}

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

	xlAudit := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=audit_logs", baseURL))
	defer xlAudit.Close()
	ipColHeader, _ := xlAudit.GetCellValue("敏感修改审计日志", "B3")
	if ipColHeader != "操作来源 IP" {
		t.Fatalf("审计日志 Excel 导出表头 B3 应为操作来源 IP，实际: %s", ipColHeader)
	}

	xlInv := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=consumable_inventory&hide_zero=1", baseURL))
	defer xlInv.Close()
	orgCell, _ := xlInv.GetCellValue("消耗品盘点统计表", "A2")
	unitCell, _ := xlInv.GetCellValue("消耗品盘点统计表", "Q2")
	if !strings.Contains(orgCell, "集团综合行政部") || unitCell != "金额单位：元" {
		t.Fatalf("盘点表 Excel 表头校验失败: org=%s, unit=%s", orgCell, unitCell)
	}

	xlLed := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=consumable_ledger", baseURL))
	defer xlLed.Close()
	hCode, _ := xlLed.GetCellValue("消耗品接续流水账", "C3")
	if hCode != "用品编码" {
		t.Fatalf("流水账 Excel 列头校验失败: C3=%s", hCode)
	}

	xlAst := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=reusable_assets", baseURL))
	defer xlAst.Close()
	astHeaders, _ := xlAst.GetRows("复用品类资产台账")
	if len(astHeaders) < 4 {
		t.Fatalf("复用品台账 Excel 数据不足")
	}

	xlLog := fetchExcel(fmt.Sprintf("%s/api/export/excel?mode=reusable_single_logs&asset_id=%d", baseURL, assetID))
	defer xlLog.Close()
	logRows, _ := xlLog.GetRows("资产全生命周期履历")
	if len(logRows) < 6 {
		t.Fatalf("单项资产履历 Excel 行数不足")
	}
}
