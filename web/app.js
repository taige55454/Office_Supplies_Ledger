let currentRole = 'admin';
let allItemsCache = [];
let consumableLedgerCache = [];
const TOKEN_STORAGE_KEY = 'office_ledger_token';

function getToken() { return localStorage.getItem(TOKEN_STORAGE_KEY) || ''; }
function setToken(t) { t ? localStorage.setItem(TOKEN_STORAGE_KEY, t) : localStorage.removeItem(TOKEN_STORAGE_KEY); }
function withToken(url) {
  const t = getToken();
  return t ? `${url}${url.includes('?') ? '&' : '?'}token=${encodeURIComponent(t)}` : url;
}

async function api(url, opts = {}) {
  const headers = opts.headers ? { ...opts.headers } : {};
  const t = getToken();
  if (t) headers['X-Auth-Token'] = t;
  const res = await fetch(url, { ...opts, headers });
  if (res.status === 401) {
    document.getElementById('login_overlay').classList.remove('hidden');
    throw new Error('请先验证访问口令');
  }
  return res.json();
}

function esc(s) {
  return String(s ?? '').replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

function openModal(id) { document.getElementById(id)?.classList.remove('hidden'); }
function closeModal(id) { document.getElementById(id)?.classList.add('hidden'); }

// ==================== 1. 登录鉴权与权限隔离 ====================
async function checkAuth() {
  const res = await fetch('/api/auth/status', { headers: getToken() ? { 'X-Auth-Token': getToken() } : {} });
  const json = await res.json();
  if (!json.authenticated) {
    openModal('login_overlay');
    return false;
  }
  currentRole = json.role || 'entry';
  applyRolePermissions(json.is_localhost);
  return true;
}

function applyRolePermissions(isLocal) {
  const badge = document.getElementById('role_badge');
  const logoutBtn = document.getElementById('btn_logout');
  if (currentRole === 'admin') {
    badge.textContent = isLocal ? '本机管理员 (全权)' : '内网管理员 (全权)';
    badge.className = 'role-pill role-admin';
    document.querySelectorAll('.admin-only').forEach(el => el.classList.remove('hidden'));
  } else {
    badge.textContent = '内网登记员 (仅可登记/上传)';
    badge.className = 'role-pill role-entry';
    document.querySelectorAll('.admin-only').forEach(el => el.classList.add('hidden'));
  }
  if (!isLocal) logoutBtn.classList.remove('hidden');
}

async function submitLogin(e) {
  e.preventDefault();
  const pass = document.getElementById('login_passcode').value.trim();
  const errEl = document.getElementById('login_err');
  const res = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ passcode: pass })
  });
  const json = await res.json();
  if (!json.success) {
    errEl.textContent = json.message || '口令错误';
    errEl.classList.remove('hidden');
    return;
  }
  setToken(json.token);
  errEl.classList.add('hidden');
  closeModal('login_overlay');
  await initApp();
}

function logout() {
  setToken('');
  location.reload();
}

// ==================== 2. 顶部模块切换 ====================
function switchTab(tab) {
  ['consumable', 'reusable', 'items', 'monthly'].forEach(t => {
    document.getElementById(`tab_${t}`).classList.toggle('active', t === tab);
    document.getElementById(`view_${t}`).classList.toggle('hidden', t !== tab);
  });
  if (tab === 'consumable') refreshConsumableView();
  if (tab === 'reusable') refreshReusableView();
  if (tab === 'items') loadItems();
  if (tab === 'monthly') loadMonthlyAttachments();
}

// ==================== 3. 基础用品字典管理 ====================
async function loadItems() {
  const nature = document.getElementById('i_filter_nature')?.value || '';
  const kw = document.getElementById('i_filter_kw')?.value || '';
  const json = await api(`/api/items?nature=${encodeURIComponent(nature)}&kw=${encodeURIComponent(kw)}`);
  if (!json.success) return;

  if (!nature && !kw) allItemsCache = json.data;
  syncCategorySelectors(json.data);

  const tbody = document.getElementById('tbody_items');
  tbody.innerHTML = json.data.map(it => {
    const natureBadge = it.nature === 'reusable'
      ? '<span class="badge bg-use">复用品(类资产)</span>'
      : '<span class="badge bg-in">消耗品</span>';
    const priceText = (it.min_price <= 0 && it.max_price <= 0)
      ? '<span style="color:#94a3b8;">暂无入库价</span>'
      : (it.min_price === it.max_price ? `¥${it.min_price.toFixed(2)}` : `¥${it.min_price.toFixed(2)} ~ ¥${it.max_price.toFixed(2)}`);
    const stockText = it.nature === 'reusable'
      ? `<strong>${it.asset_cnt}</strong> ${esc(it.base_unit)} (有效资产)`
      : `<strong>${it.stock_qty}</strong> ${esc(it.base_unit)} (约 ${(it.stock_qty / it.pack_size).toFixed(2)} ${esc(it.pack_unit || it.base_unit)})`;

    return `
      <tr>
        <td class="tc">${it.id}</td>
        <td class="tc">${esc(it.category)}</td>
        <td><strong>${esc(it.name)}</strong></td>
        <td class="tc">${natureBadge}</td>
        <td class="tc">${esc(it.base_unit)}</td>
        <td class="tc">${esc(it.pack_unit || '-')}</td>
        <td class="tc">1${esc(it.pack_unit || '包装')} = ${it.pack_size}${esc(it.base_unit)}</td>
        <td class="tc">${priceText}</td>
        <td class="tc">${stockText}</td>
        <td class="tc admin-only ${currentRole !== 'admin' ? 'hidden' : ''}">
          <button class="btn btn-outline btn-sm" onclick="openEditItemModal(${it.id})">修改(需验证)</button>
        </td>
      </tr>
    `;
  }).join('');
}

function syncCategorySelectors(items) {
  const cats = Array.from(new Set(items.map(i => i.category)));
  const datalist = document.getElementById('cat_datalist');
  if (datalist) {
    const baseCats = new Set(['办公文具', '办公耗材', '办公家具', ...cats]);
    datalist.innerHTML = Array.from(baseCats).map(c => `<option value="${esc(c)}"></option>`).join('');
  }

  ['c_filter_cat', 'r_filter_cat'].forEach(selId => {
    const sel = document.getElementById(selId);
    if (!sel) return;
    const prev = sel.value;
    sel.innerHTML = `<option value="">全部分类</option>` + cats.map(c => `<option value="${esc(c)}">${esc(c)}</option>`).join('');
    if (cats.includes(prev)) sel.value = prev;
  });

  const cItems = items.filter(i => i.nature === 'consumable');
  const cItemSel = document.getElementById('c_filter_item');
  if (cItemSel) {
    const prev = cItemSel.value;
    cItemSel.innerHTML = `<option value="">全部单品</option>` + cItems.map(i => `<option value="${i.id}">[${esc(i.category)}] ${esc(i.name)}</option>`).join('');
    cItemSel.value = prev;
  }
}

function openNewItemModal() {
  document.getElementById('modal_item_title').textContent = '新增用品基础信息';
  document.getElementById('item_edit_id').value = '';
  document.getElementById('item_category').value = '';
  document.getElementById('item_name').value = '';
  document.getElementById('item_nature').value = 'consumable';
  document.getElementById('item_nature').disabled = false;
  document.getElementById('item_base_unit').value = '';
  document.getElementById('item_pack_unit').value = '';
  document.getElementById('item_pack_size').value = '1';
  document.getElementById('item_admin_verify_box').classList.add('hidden');
  document.getElementById('item_admin_pass').required = false;
  openModal('modal_item');
}

function openEditItemModal(id) {
  const it = allItemsCache.find(x => x.id === id);
  if (!it) return;
  document.getElementById('modal_item_title').textContent = `修改用品信息：${it.name}`;
  document.getElementById('item_edit_id').value = it.id;
  document.getElementById('item_category').value = it.category;
  document.getElementById('item_name').value = it.name;
  document.getElementById('item_nature').value = it.nature;
  document.getElementById('item_nature').disabled = true;
  document.getElementById('item_base_unit').value = it.base_unit;
  document.getElementById('item_pack_unit').value = it.pack_unit;
  document.getElementById('item_pack_size').value = it.pack_size;
  document.getElementById('item_admin_verify_box').classList.remove('hidden');
  document.getElementById('item_admin_pass').value = '';
  document.getElementById('item_admin_pass').required = true;
  openModal('modal_item');
}

async function submitItemForm(e) {
  e.preventDefault();
  const editId = document.getElementById('item_edit_id').value;
  const payload = {
    category: document.getElementById('item_category').value.trim(),
    name: document.getElementById('item_name').value.trim(),
    nature: document.getElementById('item_nature').value,
    base_unit: document.getElementById('item_base_unit').value.trim(),
    pack_unit: document.getElementById('item_pack_unit').value.trim(),
    pack_size: Number(document.getElementById('item_pack_size').value) || 1,
    admin_passcode: document.getElementById('item_admin_pass').value.trim()
  };

  const url = editId ? `/api/items/${editId}` : '/api/items';
  const method = editId ? 'PUT' : 'POST';
  const json = await api(url, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!json.success) {
    alert(json.message || '操作失败');
    return;
  }
  closeModal('modal_item');
  await loadItems();
  if (editId) alert(json.message);
}

// ==================== 4. 消耗品入库/领用与双单位智能换算 ====================
function setQuickPeriod(mode) {
  const now = new Date();
  const yyyy = now.getFullYear();
  const mm = String(now.getMonth() + 1).padStart(2, '0');
  const today = `${yyyy}-${mm}-${String(now.getDate()).padStart(2, '0')}`;
  if (mode === 'month') {
    document.getElementById('c_start_date').value = `${yyyy}-${mm}-01`;
    document.getElementById('c_end_date').value = today;
  } else {
    document.getElementById('c_start_date').value = `${yyyy}-01-01`;
    document.getElementById('c_end_date').value = today;
  }
  refreshConsumableView();
}

async function refreshConsumableView() {
  const sub = document.getElementById('c_subview').value;
  const cat = document.getElementById('c_filter_cat').value;
  const itemId = document.getElementById('c_filter_item').value;
  const start = document.getElementById('c_start_date').value;
  const end = document.getElementById('c_end_date').value;

  document.getElementById('print_period_label').textContent = `统计区间：${start} 至 ${end}`;
  document.getElementById('box_c_inventory').classList.toggle('hidden', sub !== 'inventory');
  document.getElementById('box_c_ledger').classList.toggle('hidden', sub !== 'ledger');

  if (sub === 'inventory') {
    const json = await api(`/api/consumables/inventory?start_date=${start}&end_date=${end}&category=${encodeURIComponent(cat)}&item_id=${itemId}`);
    if (!json.success) return;

    document.getElementById('c_cat_cards').innerHTML = (json.category_summary || []).map(cs => `
      <div class="stat-card">
        <div class="label">📂 ${esc(cs.category)} (在库 ${cs.active_skus}/${cs.item_count} 种)</div>
        <div class="value">¥${Number(cs.total_in_amt).toLocaleString('zh-CN', { minimumFractionDigits: 2 })}</div>
      </div>
    `).join('');

    document.getElementById('tbody_c_inventory').innerHTML = (json.items || []).map(row => `
      <tr>
        <td class="tc">${esc(row.category)}</td>
        <td><strong>${esc(row.item_name)}</strong></td>
        <td class="tc">1${esc(row.pack_unit || '包装')} = ${row.pack_size}${esc(row.base_unit)}</td>
        <td class="tr"><strong>${row.opening_base_qty}</strong> ${esc(row.base_unit)}</td>
        <td class="tr">${row.opening_pack_qty} ${esc(row.pack_unit || row.base_unit)}</td>
        <td class="tr" style="color:#059669;">+${row.in_base_qty} ${esc(row.base_unit)}</td>
        <td class="tr" style="color:#d97706;">-${row.out_base_qty} ${esc(row.base_unit)}</td>
        <td class="tr" style="background:#eff6ff;"><strong>${row.closing_base_qty}</strong> ${esc(row.base_unit)}</td>
        <td class="tr" style="background:#eff6ff;">${row.closing_pack_qty} ${esc(row.pack_unit || row.base_unit)}</td>
        <td class="tr">¥${Number(row.period_in_amount).toFixed(2)}</td>
        <td class="tc">${esc(row.price_range)}</td>
      </tr>
    `).join('') || `<tr><td colspan="11" class="tc" style="padding:20px;color:#64748b;">暂无消耗品盘点数据</td></tr>`;
  } else {
    const json = await api(`/api/consumables/ledger?start_date=${start}&end_date=${end}&category=${encodeURIComponent(cat)}&item_id=${itemId}`);
    if (!json.success) return;
    consumableLedgerCache = json.data || [];

    document.getElementById('tbody_c_ledger').innerHTML = consumableLedgerCache.map(row => {
      const badge = row.tx_type === 'IN' ? '<span class="badge bg-in">入库</span>' : '<span class="badge bg-out">领用</span>';
      return `
        <tr>
          <td class="tc"><code>${esc(row.serial_no)}</code></td>
          <td class="tc">${esc(row.tx_date)}</td>
          <td class="tc">${esc(row.category)}</td>
          <td><strong>${esc(row.item_name)}</strong></td>
          <td class="tc">${badge}</td>
          <td class="tr"><strong>${row.base_qty}</strong> ${esc(row.base_unit)}</td>
          <td class="tr">${row.pack_qty} ${esc(row.pack_unit || row.base_unit)}</td>
          <td class="tr">${row.tx_type === 'IN' ? '¥' + Number(row.total_amount).toFixed(2) : '-'}</td>
          <td class="tr">${row.tx_type === 'IN' && row.unit_price > 0 ? '¥' + Number(row.unit_price).toFixed(2) : '-'}</td>
          <td class="tc">${esc(row.recipient || '-')}</td>
          <td>${esc(row.remark || '-')}</td>
          <td class="tc admin-only no-print ${currentRole !== 'admin' ? 'hidden' : ''}">
            <button class="btn btn-outline btn-sm" onclick="openEditLedgerModal(${row.id})">修正</button>
          </td>
        </tr>
      `;
    }).join('') || `<tr><td colspan="12" class="tc" style="padding:20px;color:#64748b;">该时段暂无接续流水记录</td></tr>`;
  }
}

async function openConsumableModal(txType) {
  await loadItems();
  await loadRecipientsDatalist();

  document.getElementById('c_tx_type').value = txType;
  document.getElementById('modal_c_title').textContent = txType === 'IN' ? '📥 消耗品入库登记' : '📤 消耗品领用登记';
  document.getElementById('c_form_date').value = new Date().toISOString().slice(0, 10);
  document.getElementById('c_form_base_qty').value = '';
  document.getElementById('c_form_pack_qty').value = '';
  document.getElementById('c_form_amount').value = '';
  document.getElementById('c_form_recipient').value = '';
  document.getElementById('c_form_remark').value = '';
  document.getElementById('c_pack_mismatch_tip').classList.add('hidden');

  document.getElementById('c_amount_group').classList.toggle('hidden', txType !== 'IN');
  document.getElementById('c_recipient_group').classList.toggle('hidden', txType !== 'OUT');
  document.getElementById('c_form_recipient').required = (txType === 'OUT');

  const cItems = allItemsCache.filter(i => i.nature === 'consumable');
  const cats = Array.from(new Set(cItems.map(i => i.category)));
  const catSel = document.getElementById('c_form_cat');
  catSel.innerHTML = `<option value="">全部分类</option>` + cats.map(c => `<option value="${esc(c)}">${esc(c)}</option>`).join('');
  filterConsumableItemSelect();
  openModal('modal_consumable');
}

function filterConsumableItemSelect() {
  const cat = document.getElementById('c_form_cat').value;
  const cItems = allItemsCache.filter(i => i.nature === 'consumable' && (!cat || i.category === cat));
  const itemSel = document.getElementById('c_form_item');
  itemSel.innerHTML = cItems.map(i => `<option value="${i.id}">${esc(i.name)} (${esc(i.base_unit)})</option>`).join('');
  onSelectConsumableItem();
}

function getSelectedConsumableItem() {
  const id = Number(document.getElementById('c_form_item').value);
  return allItemsCache.find(i => i.id === id);
}

function onSelectConsumableItem() {
  const it = getSelectedConsumableItem();
  const banner = document.getElementById('c_item_info_banner');
  if (!it) {
    banner.textContent = '请先在【用品基础字典】中添加消耗品';
    return;
  }
  document.getElementById('c_label_base').textContent = `基本数量 (${it.base_unit}) *`;
  document.getElementById('c_label_pack').textContent = `包装数量 (${it.pack_unit || '包装'}, 自动换算)`;
  banner.innerHTML = `规格：<strong>1 ${esc(it.pack_unit || '包装')} = ${it.pack_size} ${esc(it.base_unit)}</strong> ｜ 当前可用结存：<strong>${it.stock_qty} ${esc(it.base_unit)}</strong>`;
  onBaseQtyInput();
}

function onBaseQtyInput() {
  const it = getSelectedConsumableItem();
  if (!it) return;
  const baseQty = Number(document.getElementById('c_form_base_qty').value || 0);
  const packInput = document.getElementById('c_form_pack_qty');
  const tipBox = document.getElementById('c_pack_mismatch_tip');

  if (baseQty > 0) {
    const calcPack = baseQty / it.pack_size;
    packInput.value = Number(calcPack.toFixed(4));
    // 提示核对预设包装数量是否一致（非整数倍整箱时友好提醒）
    if (it.pack_size > 1 && Math.abs(calcPack - Math.round(calcPack)) > 0.001) {
      tipBox.textContent = `💡 提示：按预设规格 (1${it.pack_unit}=${it.pack_size}${it.base_unit}) 折算包装数为 ${calcPack.toFixed(2)} ${it.pack_unit}（非整包装），如实际包装规格不同请以【基本数量】为准！`;
      tipBox.classList.remove('hidden');
    } else {
      tipBox.classList.add('hidden');
    }
  } else {
    packInput.value = '';
    tipBox.classList.add('hidden');
  }
  onTotalAmountInput();
}

function onPackQtyInput() {
  const it = getSelectedConsumableItem();
  if (!it) return;
  const packQty = Number(document.getElementById('c_form_pack_qty').value || 0);
  const baseInput = document.getElementById('c_form_base_qty');
  if (packQty > 0) {
    baseInput.value = Number((packQty * it.pack_size).toFixed(4));
  } else {
    baseInput.value = '';
  }
  document.getElementById('c_pack_mismatch_tip').classList.add('hidden');
  onTotalAmountInput();
}

function onTotalAmountInput() {
  if (document.getElementById('c_tx_type').value !== 'IN') return;
  const it = getSelectedConsumableItem();
  const baseQty = Number(document.getElementById('c_form_base_qty').value || 0);
  const totalAmt = Number(document.getElementById('c_form_amount').value || 0);
  const banner = document.getElementById('c_item_info_banner');
  if (it && baseQty > 0 && totalAmt > 0) {
    const unitP = (totalAmt / baseQty).toFixed(2);
    banner.innerHTML = `规格：1 ${esc(it.pack_unit || '包装')} = ${it.pack_size} ${esc(it.base_unit)} ｜ 当前结存：${it.stock_qty} ${esc(it.base_unit)} ｜ <strong>本次折算单价：¥${unitP} / ${esc(it.base_unit)}</strong>`;
  }
}

async function submitConsumableForm(e) {
  e.preventDefault();
  const payload = {
    item_id: Number(document.getElementById('c_form_item').value),
    tx_type: document.getElementById('c_tx_type').value,
    tx_date: document.getElementById('c_form_date').value,
    base_qty: Number(document.getElementById('c_form_base_qty').value),
    total_amount: Number(document.getElementById('c_form_amount').value || 0),
    recipient: document.getElementById('c_form_recipient').value.trim(),
    remark: document.getElementById('c_form_remark').value.trim()
  };

  const json = await api('/api/consumables/ledger', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!json.success) {
    alert(json.message || '登记失败');
    return;
  }
  closeModal('modal_consumable');
  await loadItems();
  await refreshConsumableView();
}

function openEditLedgerModal(id) {
  const row = consumableLedgerCache.find(r => r.id === id);
  if (!row) return;
  document.getElementById('edit_ledger_title').textContent = `修正流水：${row.serial_no} (${row.item_name})`;
  document.getElementById('el_id').value = row.id;
  document.getElementById('el_date').value = row.tx_date;
  document.getElementById('el_base_qty').value = row.base_qty;
  document.getElementById('el_amount').value = row.total_amount;
  document.getElementById('el_recipient').value = row.recipient;
  document.getElementById('el_remark').value = row.remark;
  document.getElementById('el_admin_pass').value = '';
  document.getElementById('el_amt_box').classList.toggle('hidden', row.tx_type !== 'IN');
  document.getElementById('el_recip_box').classList.toggle('hidden', row.tx_type !== 'OUT');
  openModal('modal_edit_ledger');
}

async function submitEditLedgerForm(e) {
  e.preventDefault();
  const id = document.getElementById('el_id').value;
  const payload = {
    tx_date: document.getElementById('el_date').value,
    base_qty: Number(document.getElementById('el_base_qty').value),
    total_amount: Number(document.getElementById('el_amount').value || 0),
    recipient: document.getElementById('el_recipient').value.trim(),
    remark: document.getElementById('el_remark').value.trim(),
    admin_passcode: document.getElementById('el_admin_pass').value.trim()
  };
  const json = await api(`/api/consumables/ledger/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!json.success) {
    alert(json.message || '修正失败');
    return;
  }
  closeModal('modal_edit_ledger');
  await loadItems();
  await refreshConsumableView();
}

function exportConsumableExcel() {
  const sub = document.getElementById('c_subview').value;
  const mode = sub === 'ledger' ? 'consumable_ledger' : 'consumable_inventory';
  const start = document.getElementById('c_start_date').value;
  const end = document.getElementById('c_end_date').value;
  window.location.href = withToken(`/api/export/excel?mode=${mode}&start_date=${start}&end_date=${end}`);
}

// ==================== 5. 复用品（类资产全生命周期管理） ====================
async function refreshReusableView() {
  const sub = document.getElementById('r_subview').value;
  const cat = document.getElementById('r_filter_cat').value;
  const status = document.getElementById('r_filter_status').value;
  const kw = document.getElementById('r_filter_kw').value.trim();

  document.getElementById('box_r_assets').classList.toggle('hidden', sub !== 'assets');
  document.getElementById('box_r_logs').classList.toggle('hidden', sub !== 'logs');

  if (sub === 'assets') {
    const json = await api(`/api/reusables/assets?category=${encodeURIComponent(cat)}&status=${encodeURIComponent(status)}&kw=${encodeURIComponent(kw)}`);
    if (!json.success) return;
    const s = json.summary;
    document.getElementById('r_summary_cards').innerHTML = `
      <div class="stat-card"><div class="label">📦 类资产总件数</div><div class="value">${s.total_count} 件</div></div>
      <div class="stat-card"><div class="label">🟢 在用资产</div><div class="value" style="color:#1d4ed8;">${s.in_use_count} 件</div></div>
      <div class="stat-card"><div class="label">⚪ 闲置在库</div><div class="value">${s.idle_count} 件</div></div>
      <div class="stat-card"><div class="label">🔴 已登记报废</div><div class="value" style="color:#dc2626;">${s.scrap_count} 件</div></div>
      <div class="stat-card"><div class="label">💰 有效资产净值</div><div class="value">¥${Number(s.active_value).toLocaleString()}</div></div>
    `;

    document.getElementById('tbody_r_assets').innerHTML = (json.data || []).map(a => {
      let stBadge = '<span class="badge bg-idle">闲置</span>';
      if (a.status === '在用') stBadge = '<span class="badge bg-use">在用</span>';
      if (a.status === '报废') stBadge = '<span class="badge bg-scrap">报废</span>';

      const imgLink = a.image_path ? `<a href="${withToken('/attachments/' + encodeURI(a.image_path))}" target="_blank">🖼️ 实物图</a>` : '';
      const scrapLink = a.scrap_file_path ? ` ｜ <a href="${withToken('/attachments/' + encodeURI(a.scrap_file_path))}" target="_blank" style="color:#dc2626;">📄 报废单</a>` : '';

      let actionBtns = `<button class="btn btn-outline btn-sm" onclick="viewAssetLogs(${a.id}, '${esc(a.asset_no)}')">履历</button>`;
      if (a.status === '闲置') {
        actionBtns += ` <button class="btn btn-primary btn-sm" onclick="openReusableActionModal(${a.id}, 'ASSIGN')">领用</button>`;
        actionBtns += ` <button class="btn btn-danger btn-sm" onclick="openReusableActionModal(${a.id}, 'SCRAP')">报废</button>`;
      } else if (a.status === '在用') {
        actionBtns += ` <button class="btn btn-warning btn-sm" onclick="openReusableActionModal(${a.id}, 'TRANSFER')">变更使用人</button>`;
        actionBtns += ` <button class="btn btn-danger btn-sm" onclick="openReusableActionModal(${a.id}, 'SCRAP')">报废</button>`;
      }

      return `
        <tr>
          <td class="tc"><code>${esc(a.asset_no)}</code></td>
          <td class="tc">${esc(a.category)}</td>
          <td><strong>${esc(a.item_name)}</strong></td>
          <td>${esc(a.sub_name || '-')}</td>
          <td class="tc">${esc(a.purchase_date)}</td>
          <td class="tr">¥${Number(a.unit_price).toFixed(2)}</td>
          <td class="tc">${a.expected_years}年</td>
          <td class="tc">${stBadge}</td>
          <td class="tc"><strong>${esc(a.current_user || '-')}</strong></td>
          <td class="tc">${imgLink}${scrapLink}</td>
          <td class="tc">${actionBtns}</td>
        </tr>
      `;
    }).join('') || `<tr><td colspan="11" class="tc" style="padding:20px;color:#64748b;">暂无复用品类资产记录</td></tr>`;
  } else {
    const json = await api(`/api/reusables/logs?category=${encodeURIComponent(cat)}`);
    if (!json.success) return;
    document.getElementById('tbody_r_logs').innerHTML = (json.data || []).map(l => `
      <tr>
        <td class="tc"><code>${esc(l.log_no)}</code></td>
        <td class="tc"><code>${esc(l.asset_no)}</code></td>
        <td class="tc">${esc(l.category)}</td>
        <td><strong>${esc(l.item_name)}</strong></td>
        <td>${esc(l.sub_name || '-')}</td>
        <td class="tc"><span class="badge bg-use">${esc(l.action_type)}</span></td>
        <td class="tc">${esc(l.action_date)}</td>
        <td class="tc">${esc(l.operator_or_user || '-')}</td>
        <td>${esc(l.detail)}</td>
        <td class="tc">${l.attachment_path ? `<a href="${withToken('/attachments/' + encodeURI(l.attachment_path))}" target="_blank">📎 查看凭证</a>` : '-'}</td>
      </tr>
    `).join('');
  }
}

async function openReusableInModal() {
  await loadItems();
  const rItems = allItemsCache.filter(i => i.nature === 'reusable');
  document.getElementById('ri_item_id').innerHTML = rItems.map(i => `<option value="${i.id}">[${esc(i.category)}] ${esc(i.name)}</option>`).join('');
  document.getElementById('ri_sub_name').value = '';
  document.getElementById('ri_qty').value = '1';
  document.getElementById('ri_total_amount').value = '0';
  document.getElementById('ri_purchase_date').value = new Date().toISOString().slice(0, 10);
  document.getElementById('ri_expected_years').value = '10';
  document.getElementById('ri_image_file').value = '';
  document.getElementById('ri_remark').value = '';
  openModal('modal_reusable_in');
}

async function submitReusableInForm(e) {
  e.preventDefault();
  const fd = new FormData();
  fd.append('item_id', document.getElementById('ri_item_id').value);
  fd.append('sub_name', document.getElementById('ri_sub_name').value.trim());
  fd.append('quantity', document.getElementById('ri_qty').value);
  fd.append('total_amount', document.getElementById('ri_total_amount').value);
  fd.append('purchase_date', document.getElementById('ri_purchase_date').value);
  fd.append('expected_years', document.getElementById('ri_expected_years').value);
  fd.append('remark', document.getElementById('ri_remark').value.trim());
  const f = document.getElementById('ri_image_file').files[0];
  if (f) fd.append('image_file', f);

  const json = await api('/api/reusables/assets', { method: 'POST', body: fd });
  if (!json.success) {
    alert(json.message || '建档失败');
    return;
  }
  closeModal('modal_reusable_in');
  alert(json.message);
  await loadItems();
  await refreshReusableView();
}

async function openReusableActionModal(assetId, action) {
  await loadRecipientsDatalist();
  document.getElementById('ra_asset_id').value = assetId;
  document.getElementById('ra_action').value = action;
  document.getElementById('ra_date').value = new Date().toISOString().slice(0, 10);
  document.getElementById('ra_user').value = '';
  document.getElementById('ra_remark').value = '';
  document.getElementById('ra_scrap_file').value = '';

  const titles = { ASSIGN: '🪑 复用品领用分配', TRANSFER: '🔄 变更资产使用人', SCRAP: '🗑️ 资产报废登记 (需上传凭证)' };
  document.getElementById('ra_title').textContent = titles[action] || '资产流转';
  document.getElementById('ra_user_box').classList.toggle('hidden', action === 'SCRAP');
  document.getElementById('ra_user').required = (action !== 'SCRAP');
  document.getElementById('ra_scrap_file_box').classList.toggle('hidden', action !== 'SCRAP');
  document.getElementById('ra_scrap_file').required = (action === 'SCRAP');
  openModal('modal_reusable_action');
}

async function submitReusableActionForm(e) {
  e.preventDefault();
  const assetId = document.getElementById('ra_asset_id').value;
  const fd = new FormData();
  fd.append('action', document.getElementById('ra_action').value);
  fd.append('action_date', document.getElementById('ra_date').value);
  fd.append('user', document.getElementById('ra_user').value.trim());
  fd.append('remark', document.getElementById('ra_remark').value.trim());
  const f = document.getElementById('ra_scrap_file').files[0];
  if (f) fd.append('scrap_file', f);

  const json = await api(`/api/reusables/assets/${assetId}/transition`, { method: 'POST', body: fd });
  if (!json.success) {
    alert(json.message || '流转失败');
    return;
  }
  closeModal('modal_reusable_action');
  await refreshReusableView();
}

async function viewAssetLogs(assetId, assetNo) {
  document.getElementById('al_title').textContent = `资产履历留痕：${assetNo}`;
  const json = await api(`/api/reusables/assets/${assetId}/logs`);
  if (!json.success) return;
  document.getElementById('tbody_single_asset_logs').innerHTML = (json.data || []).map(l => `
    <tr>
      <td class="tc"><code>${esc(l.log_no)}</code></td>
      <td class="tc"><span class="badge bg-use">${esc(l.action_type)}</span></td>
      <td class="tc">${esc(l.action_date)}</td>
      <td class="tc">${esc(l.operator_or_user || '-')}</td>
      <td>${esc(l.detail)}</td>
      <td class="tc">${l.attachment_path ? `<a href="${withToken('/attachments/' + encodeURI(l.attachment_path))}" target="_blank">📎 查看</a>` : '-'}</td>
    </tr>
  `).join('');
  openModal('modal_asset_logs');
}

function exportReusableExcel() {
  window.location.href = withToken('/api/export/excel?mode=reusable_assets');
}

// ==================== 6. 月度手写登记附件管理 ====================
async function loadMonthlyAttachments() {
  const ym = document.getElementById('m_filter_ym')?.value || '';
  const json = await api(`/api/monthly-attachments?year_month=${encodeURIComponent(ym)}`);
  if (!json.success) return;
  document.getElementById('tbody_monthly').innerHTML = (json.data || []).map(m => `
    <tr>
      <td class="tc"><strong>${esc(m.year_month)}</strong></td>
      <td>${esc(m.title)}</td>
      <td>${esc(m.original_name)}</td>
      <td>${esc(m.remark || '-')}</td>
      <td class="tc">${esc(m.uploaded_at)}</td>
      <td class="tc">
        <a class="btn btn-primary btn-sm" href="${withToken('/attachments/' + encodeURI(m.file_path))}" target="_blank">预览查看</a>
        <a class="btn btn-outline btn-sm" href="${withToken('/attachments/' + encodeURI(m.file_path) + '?download=1')}">下载</a>
        <button class="btn btn-danger btn-sm admin-only ${currentRole !== 'admin' ? 'hidden' : ''}" onclick="deleteMonthlyAttachment(${m.id})">删除(需验证)</button>
      </td>
    </tr>
  `).join('') || `<tr><td colspan="6" class="tc" style="padding:20px;color:#64748b;">暂无月度手写登记附件</td></tr>`;
}

function openMonthlyUploadModal() {
  document.getElementById('mu_ym').value = new Date().toISOString().slice(0, 7);
  document.getElementById('mu_title').value = '';
  document.getElementById('mu_file').value = '';
  document.getElementById('mu_remark').value = '';
  openModal('modal_monthly_upload');
}

async function submitMonthlyUpload(e) {
  e.preventDefault();
  const fd = new FormData();
  fd.append('year_month', document.getElementById('mu_ym').value);
  fd.append('title', document.getElementById('mu_title').value.trim());
  fd.append('remark', document.getElementById('mu_remark').value.trim());
  const f = document.getElementById('mu_file').files[0];
  if (f) fd.append('file', f);

  const json = await api('/api/monthly-attachments', { method: 'POST', body: fd });
  if (!json.success) {
    alert(json.message || '上传失败');
    return;
  }
  closeModal('modal_monthly_upload');
  await loadMonthlyAttachments();
}

async function deleteMonthlyAttachment(id) {
  const pass = prompt('⚠️ 删除月度原始手写登记附件需要管理员验证，请输入管理员口令：');
  if (!pass) return;
  const json = await api(`/api/monthly-attachments/${id}?admin_passcode=${encodeURIComponent(pass)}`, { method: 'DELETE' });
  if (!json.success) {
    alert(json.message || '删除失败');
    return;
  }
  await loadMonthlyAttachments();
}

// ==================== 7. 年度结转、端口与口令设置、审计日志 ====================
async function openCarryoverModal() {
  document.getElementById('co_year').value = new Date().getFullYear();
  document.getElementById('co_pass').value = '';
  const json = await api('/api/carryover');
  if (json.success) {
    document.getElementById('co_history_box').innerHTML = '已结转历史年份：' +
      ((json.data || []).map(x => `<strong>${x.carry_year}年</strong>(${x.item_count}项)`).join('、') || '暂无');
  }
  openModal('modal_carryover');
}

async function submitCarryover(e) {
  e.preventDefault();
  const payload = {
    carry_year: Number(document.getElementById('co_year').value),
    admin_passcode: document.getElementById('co_pass').value.trim()
  };
  const json = await api('/api/carryover', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!json.success) {
    alert(json.message || '结转失败');
    return;
  }
  closeModal('modal_carryover');
  alert(json.message);
  await refreshConsumableView();
}

async function openSettingsModal() {
  const json = await api('/api/auth/status');
  document.getElementById('set_port').value = json.port || 8088;
  document.getElementById('set_allow_lan').checked = json.allow_lan !== false;
  document.getElementById('set_admin_pass').value = json.admin_passcode || '888888';
  document.getElementById('set_entry_pass').value = json.entry_passcode || '666666';
  document.getElementById('set_lan_urls').innerHTML = (json.lan_urls || []).join('<br/>') || `http://127.0.0.1:${json.port}`;
  openModal('modal_settings');
}

async function submitSettings(e) {
  e.preventDefault();
  const newPort = Number(document.getElementById('set_port').value);
  const payload = {
    port: newPort,
    allow_lan: document.getElementById('set_allow_lan').checked,
    admin_passcode: document.getElementById('set_admin_pass').value.trim(),
    entry_passcode: document.getElementById('set_entry_pass').value.trim()
  };
  const json = await api('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  if (!json.success) {
    alert(json.message || '保存失败');
    return;
  }
  closeModal('modal_settings');
  if (json.need_restart) {
    alert(`端口已热切换为 ${newPort}，即将自动跳转到新端口地址！`);
    setTimeout(() => {
      window.location.href = `${window.location.protocol}//${window.location.hostname}:${newPort}`;
    }, 600);
  } else {
    alert('配置已保存！');
  }
}

async function openAuditModal() {
  const json = await api('/api/audit-logs');
  if (!json.success) return;
  document.getElementById('tbody_audit').innerHTML = (json.data || []).map(a => `
    <tr>
      <td class="tc">${esc(a.created_at)}</td>
      <td class="tc"><strong>${esc(a.action)}</strong></td>
      <td>${esc(a.target_info)}</td>
      <td>${esc(a.detail)}</td>
    </tr>
  `).join('');
  openModal('modal_audit');
}

async function loadRecipientsDatalist() {
  const json = await api('/api/recipients');
  if (json.success) {
    document.getElementById('recipients_datalist').innerHTML = (json.data || []).map(n => `<option value="${esc(n)}"></option>`).join('');
  }
}

// ==================== 8. 启动初始化 ====================
async function initApp() {
  setQuickPeriod('year');
  await loadItems();
  await refreshConsumableView();
}

window.addEventListener('DOMContentLoaded', async () => {
  if (await checkAuth()) {
    await initApp();
  }
});
