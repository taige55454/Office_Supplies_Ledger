let authState = {
  role: 'guest',
  token: localStorage.getItem('ledger_token') || '',
  is_localhost: false,
  port: 8088,
  lan_urls: [],
  allow_lan: true,
  org_name: '综合行政部'
};

let allItems = [];
let allRecipients = [];
let cachedReusableAssets = [];
let cachedInventoryItems = [];
let currentInspectingAsset = null;
let currentPrintMode = 'consumable_inventory';

function todayStr() {
  const d = new Date();
  return d.toISOString().slice(0, 10);
}

function yearStartStr() {
  return `${new Date().getFullYear()}-01-01`;
}

async function apiFetch(url, options = {}) {
  const headers = options.headers || {};
  if (authState.token) {
    headers['X-Auth-Token'] = authState.token;
  }
  options.headers = headers;
  const resp = await fetch(url, options);
  if (resp.status === 401) {
    document.getElementById('lock-overlay').classList.remove('hidden');
  }
  return resp;
}

document.addEventListener('DOMContentLoaded', async () => {
  document.getElementById('inv-start-date').value = yearStartStr();
  document.getElementById('inv-end-date').value = todayStr();

  document.querySelectorAll('.tab-btn').forEach(btn => {
    btn.addEventListener('click', () => {
      document.querySelectorAll('.tab-btn').forEach(b => b.classList.remove('active'));
      document.querySelectorAll('.tab-panel').forEach(p => p.classList.remove('active'));
      btn.classList.add('active');
      document.getElementById(btn.dataset.tab).classList.add('active');

      if (btn.dataset.tab === 'tab-reusables') loadReusableAssets();
      if (btn.dataset.tab === 'tab-monthly') loadMonthlyAttachments();
      if (btn.dataset.tab === 'tab-audit') loadAuditLogs();
    });
  });

  document.getElementById('btn-login').addEventListener('click', doLogin);
  document.getElementById('login-passcode').addEventListener('keydown', e => {
    if (e.key === 'Enter') doLogin();
  });

  await checkAuthAndInit();
});

async function checkAuthAndInit() {
  const resp = await apiFetch('/api/auth/status');
  const data = await resp.json();
  if (!data.authenticated) {
    document.getElementById('lock-overlay').classList.remove('hidden');
    return;
  }
  document.getElementById('lock-overlay').classList.add('hidden');
  authState = { ...authState, ...data };

  const badge = document.getElementById('role-badge');
  if (authState.role === 'admin') {
    badge.textContent = authState.is_localhost ? '👑 本机管理员 (全权限)' : '👑 管理员 (全权限)';
    badge.className = 'role-badge admin';
    document.querySelectorAll('.admin-only').forEach(el => el.classList.remove('hidden'));
    document.getElementById('btn-open-settings').classList.remove('hidden');
  } else {
    badge.textContent = '📝 内网登记员 (只读/录入)';
    badge.className = 'role-badge entry';
    document.querySelectorAll('.admin-only').forEach(el => el.classList.add('hidden'));
    document.getElementById('btn-open-settings').classList.add('hidden');
  }

  const lanHint = authState.lan_urls && authState.lan_urls.length > 0
    ? `内网访问: ${authState.lan_urls.join(' | ')}`
    : `本机端口: ${authState.port}`;
  document.getElementById('org-subtitle').textContent = `单位：${authState.org_name || '综合行政部'}  ·  ${lanHint}`;

  await loadItems();
  await loadRecipients();
  await loadConsumableData();
}

async function doLogin() {
  const pass = document.getElementById('login-passcode').value;
  const errEl = document.getElementById('login-error');
  const resp = await fetch('/api/auth/login', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ passcode: pass })
  });
  const res = await resp.json();
  if (!res.success) {
    errEl.textContent = res.message || '验证失败';
    errEl.classList.remove('hidden');
    return;
  }
  authState.token = res.token;
  localStorage.setItem('ledger_token', res.token);
  errEl.classList.add('hidden');
  await checkAuthAndInit();
}

function closeModal(id) {
  document.getElementById(id).classList.add('hidden');
}

// ==================== 1. 基础用品字典管理 ====================

function openDictModal() {
  renderDictTable();
  document.getElementById('modal-dict').classList.remove('hidden');
}

async function loadItems() {
  const resp = await apiFetch('/api/items');
  const res = await resp.json();
  if (!res.success) return;
  allItems = res.data || [];

  const cats = [...new Set(allItems.map(i => i.category))];
  const catOptions = '<option value="">全部分类</option>' + cats.map(c => `<option value="${c}">${c}</option>`).join('');
  document.getElementById('inv-category').innerHTML = catOptions;
  document.getElementById('ra-category').innerHTML = catOptions;
  document.getElementById('cat-datalist').innerHTML = cats.map(c => `<option value="${c}">`).join('');

  onInvCategoryChange();
  renderDictTable();
}

function onInvCategoryChange() {
  const cat = document.getElementById('inv-category').value;
  const consumables = allItems.filter(i => i.nature === 'consumable' && (!cat || i.category === cat));
  document.getElementById('inv-item-id').innerHTML =
    '<option value="">全部品名</option>' + consumables.map(i => `<option value="${i.id}">${i.name} (${i.base_unit})</option>`).join('');
}

function renderDictTable() {
  const nat = document.getElementById('dict-nature').value;
  const kw = document.getElementById('dict-kw').value.trim().toLowerCase();
  const filtered = allItems.filter(i => {
    if (nat && i.nature !== nat) return false;
    if (kw && !i.name.toLowerCase().includes(kw) && !i.category.toLowerCase().includes(kw)) return false;
    return true;
  });

  const tbody = document.getElementById('tbody-dict');
  tbody.innerHTML = filtered.map(it => {
    const spec = it.pack_unit ? `1${it.pack_unit}=${it.pack_size}${it.base_unit}` : `1${it.base_unit}`;
    const stockTxt = it.nature === 'consumable'
      ? `${it.stock_qty} (价值 ${(it.stock_value || 0).toFixed(2)})`
      : `在库/用 ${it.asset_cnt}`;
    const priceTxt = it.min_price > 0
      ? (it.min_price === it.max_price ? `${it.min_price.toFixed(2)}` : `${it.min_price.toFixed(2)} ~ ${it.max_price.toFixed(2)}`)
      : '暂无';
    const priceBtn = it.nature === 'consumable' && it.min_price > 0
      ? `<button class="price-link" onclick="openPriceDistModal(${it.id})">${priceTxt} 📊</button>`
      : priceTxt;
    const editBtn = authState.role === 'admin'
      ? `<button class="btn btn-sm btn-outline" onclick="openEditItemModal(${it.id})">✏️ 修改</button>`
      : `<span class="hint-text">只读</span>`;
    return `
      <tr>
        <td><strong>${it.category}</strong></td>
        <td>${it.name}</td>
        <td><span class="badge ${it.nature === 'consumable' ? 'badge-idle' : 'badge-use'}">${it.nature === 'consumable' ? '消耗品' : '复用品'}</span></td>
        <td>${it.base_unit}</td>
        <td>${it.pack_unit || '-'}</td>
        <td>${spec}</td>
        <td>${stockTxt}</td>
        <td>${priceBtn}</td>
        <td>${editBtn}</td>
      </tr>`;
  }).join('');
}

function openNewItemModal() {
  document.getElementById('new-item-cat').value = '';
  document.getElementById('new-item-name').value = '';
  document.getElementById('new-item-base-unit').value = '';
  document.getElementById('new-item-pack-unit').value = '';
  document.getElementById('new-item-pack-size').value = '1';
  document.getElementById('modal-new-item').classList.remove('hidden');
}

async function submitNewItem() {
  const payload = {
    category: document.getElementById('new-item-cat').value.trim(),
    nature: document.getElementById('new-item-nature').value,
    name: document.getElementById('new-item-name').value.trim(),
    base_unit: document.getElementById('new-item-base-unit').value.trim(),
    pack_unit: document.getElementById('new-item-pack-unit').value.trim(),
    pack_size: parseInt(document.getElementById('new-item-pack-size').value, 10) || 1
  };
  const resp = await apiFetch('/api/items', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  closeModal('modal-new-item');
  await loadItems();
  await loadConsumableData();
}

function openEditItemModal(id) {
  const it = allItems.find(x => x.id === id);
  if (!it) return;
  document.getElementById('edit-item-id').value = it.id;
  document.getElementById('edit-item-cat').value = it.category;
  document.getElementById('edit-item-name').value = it.name;
  document.getElementById('edit-item-base-unit').value = it.base_unit;
  document.getElementById('edit-item-pack-unit').value = it.pack_unit;
  document.getElementById('edit-item-pack-size').value = it.pack_size;
  document.getElementById('edit-item-pass').value = '';
  document.getElementById('modal-edit-item').classList.remove('hidden');
}

async function submitEditItem() {
  const id = document.getElementById('edit-item-id').value;
  const payload = {
    category: document.getElementById('edit-item-cat').value.trim(),
    name: document.getElementById('edit-item-name').value.trim(),
    base_unit: document.getElementById('edit-item-base-unit').value.trim(),
    pack_unit: document.getElementById('edit-item-pack-unit').value.trim(),
    pack_size: parseInt(document.getElementById('edit-item-pack-size').value, 10) || 1,
    admin_passcode: document.getElementById('edit-item-pass').value
  };
  const resp = await apiFetch(`/api/items/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  alert(res.message);
  closeModal('modal-edit-item');
  await loadItems();
  await loadConsumableData();
}

// ==================== 2. 消耗品出入库 ====================

async function loadRecipients() {
  const resp = await apiFetch('/api/recipients');
  const res = await resp.json();
  if (res.success) {
    allRecipients = res.data || [];
    document.getElementById('recipient-datalist').innerHTML = allRecipients.map(r => `<option value="${r}">`).join('');
  }
}

function openConsumableTxModal(type) {
  document.getElementById('tx-type').value = type;
  document.getElementById('modal-tx-title').textContent = type === 'IN' ? '📥 消耗品入库登记' : '📤 消耗品领用登记';
  document.getElementById('tx-date').value = todayStr();
  document.getElementById('tx-base-qty').value = '';
  document.getElementById('tx-pack-qty').value = '';
  document.getElementById('tx-total-amount').value = '';
  document.getElementById('tx-recipient').value = '';
  document.getElementById('tx-remark').value = '';
  document.getElementById('tx-unit-price-hint').textContent = '';
  document.getElementById('tx-pack-warning').classList.add('hidden');
  document.getElementById('tx-override-box').classList.add('hidden');
  document.getElementById('tx-override-pass').value = '';
  document.getElementById('tx-custom-serial').value = '';

  if (type === 'IN') {
    document.getElementById('tx-amount-group').classList.remove('hidden');
    document.getElementById('tx-recipient-group').classList.add('hidden');
  } else {
    document.getElementById('tx-amount-group').classList.add('hidden');
    document.getElementById('tx-recipient-group').classList.remove('hidden');
  }

  const consumables = allItems.filter(i => i.nature === 'consumable');
  const cats = [...new Set(consumables.map(i => i.category))];
  document.getElementById('tx-cat-filter').innerHTML = '<option value="">全部分类</option>' + cats.map(c => `<option value="${c}">${c}</option>`).join('');
  filterTxItems();
  document.getElementById('modal-consumable-tx').classList.remove('hidden');
}

function filterTxItems() {
  const cat = document.getElementById('tx-cat-filter').value;
  const list = allItems.filter(i => i.nature === 'consumable' && (!cat || i.category === cat));
  document.getElementById('tx-item-id').innerHTML = list.map(i => `<option value="${i.id}">${i.name} (${i.base_unit})</option>`).join('');
  onTxItemChange();
}

function onTxItemChange() {
  const id = parseInt(document.getElementById('tx-item-id').value, 10);
  const it = allItems.find(x => x.id === id);
  if (!it) return;
  const spec = it.pack_unit ? `1 ${it.pack_unit} = ${it.pack_size} ${it.base_unit}` : `最小单位：${it.base_unit}`;
  document.getElementById('tx-item-spec-banner').innerHTML =
    `规格：<strong>${spec}</strong> &nbsp;|&nbsp; 当前可用结存：<strong>${it.stock_qty} ${it.base_unit}</strong> &nbsp;|&nbsp; 在库价值：<strong>${(it.stock_value || 0).toFixed(2)} 元</strong>`;
  document.getElementById('tx-base-label').textContent = `入库领用数量 (${it.base_unit}，必须为整数) *`;
  document.getElementById('tx-pack-label').textContent = `参考包装数量 (${it.pack_unit || '件'}，填写可自动算基本数量)`;
  onBaseQtyInput();
}

function onBaseQtyInput() {
  const id = parseInt(document.getElementById('tx-item-id').value, 10);
  const it = allItems.find(x => x.id === id);
  if (!it) return;
  const rawVal = document.getElementById('tx-base-qty').value;
  const bQty = parseInt(rawVal, 10);
  if (!rawVal || isNaN(bQty) || bQty <= 0) {
    document.getElementById('tx-pack-qty').value = '';
    document.getElementById('tx-pack-warning').classList.add('hidden');
    calcTxUnitPrice();
    return;
  }
  const pQty = Number((bQty / it.pack_size).toFixed(2));
  document.getElementById('tx-pack-qty').value = pQty;

  const warnEl = document.getElementById('tx-pack-warning');
  if (it.pack_size > 1 && bQty % it.pack_size !== 0) {
    warnEl.textContent = `💡 提示：${bQty} ${it.base_unit} 折合约 ${pQty} ${it.pack_unit}，记账时系统自动执行累计包装尾差倒挤对齐。`;
    warnEl.classList.remove('hidden');
  } else {
    warnEl.classList.add('hidden');
  }
  calcTxUnitPrice();
}

function onPackQtyInput() {
  const id = parseInt(document.getElementById('tx-item-id').value, 10);
  const it = allItems.find(x => x.id === id);
  if (!it) return;
  const pQty = parseFloat(document.getElementById('tx-pack-qty').value);
  if (isNaN(pQty) || pQty <= 0) {
    document.getElementById('tx-base-qty').value = '';
    document.getElementById('tx-pack-warning').classList.add('hidden');
    calcTxUnitPrice();
    return;
  }
  const bQty = Math.max(1, Math.round(pQty * it.pack_size));
  document.getElementById('tx-base-qty').value = bQty;

  const warnEl = document.getElementById('tx-pack-warning');
  if (Math.abs(pQty * it.pack_size - bQty) > 0.001) {
    warnEl.textContent = `💡 已按包装规格自动取整为基本数量【${bQty} ${it.base_unit}】（最小计量单位不可取小数）。`;
    warnEl.classList.remove('hidden');
  } else {
    warnEl.classList.add('hidden');
  }
  calcTxUnitPrice();
}

function calcTxUnitPrice() {
  const bQty = parseInt(document.getElementById('tx-base-qty').value, 10) || 0;
  const amt = parseFloat(document.getElementById('tx-total-amount').value) || 0;
  const hint = document.getElementById('tx-unit-price-hint');
  if (document.getElementById('tx-type').value === 'IN' && bQty > 0 && amt > 0) {
    hint.textContent = `💰 本次折算入库单价：${(amt / bQty).toFixed(4)} 元 / 基本单位`;
  } else {
    hint.textContent = '';
  }
}

async function submitConsumableTx() {
  const rawQty = document.getElementById('tx-base-qty').value;
  const bQty = Number(rawQty);
  if (!rawQty || !Number.isInteger(bQty) || bQty <= 0) {
    alert('入库领用数量必须为大于 0 的整数！');
    return;
  }

  const payload = {
    item_id: parseInt(document.getElementById('tx-item-id').value, 10),
    tx_type: document.getElementById('tx-type').value,
    tx_date: document.getElementById('tx-date').value,
    base_qty: bQty,
    total_amount: parseFloat(document.getElementById('tx-total-amount').value) || 0,
    recipient: document.getElementById('tx-recipient').value.trim(),
    remark: document.getElementById('tx-remark').value.trim(),
    custom_serial_no: document.getElementById('tx-custom-serial').value.trim(),
    admin_passcode: document.getElementById('tx-override-pass').value.trim()
  };

  const resp = await apiFetch('/api/consumables/ledger', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const res = await resp.json();
  if (!res.success) {
    if (res.need_override) {
      document.getElementById('tx-override-msg').textContent = '⚠️ ' + res.message;
      document.getElementById('tx-override-box').classList.remove('hidden');
      document.getElementById('tx-override-pass').focus();
      return;
    }
    alert(res.message);
    return;
  }
  closeModal('modal-consumable-tx');
  await loadItems();
  await loadRecipients();
  await loadConsumableData();
}

// ==================== 3. 消耗品在库价值盘点表与接续流水账 ====================

async function loadConsumableData() {
  const start = document.getElementById('inv-start-date').value;
  const end = document.getElementById('inv-end-date').value;
  const cat = document.getElementById('inv-category').value;
  const itemId = document.getElementById('inv-item-id').value;
  const hideZero = document.getElementById('inv-hide-zero').checked ? '1' : '0';

  const qs = new URLSearchParams({ start_date: start, end_date: end, category: cat, item_id: itemId, hide_zero: hideZero });
  const resp = await apiFetch(`/api/consumables/inventory?${qs.toString()}`);
  const res = await resp.json();
  if (!res.success) return;

  cachedInventoryItems = res.items || [];

  const catCards = document.getElementById('consumable-cat-cards');
  const totalStockValue = (res.category_summary || []).reduce((acc, c) => acc + (c.total_closing_value || 0), 0);

  catCards.innerHTML = `
    <div class="stat-card" style="border-left:4px solid #2563eb;">
      <div class="stat-title">💰 全部在库物资实时总价值 (元)</div>
      <div class="stat-val" style="color:#1d4ed8;">${totalStockValue.toFixed(2)}</div>
    </div>
  ` + (res.category_summary || []).map(c => `
    <div class="stat-card">
      <div class="stat-title">📁 ${c.category}（在库 ${c.active_skus}/${c.item_count} 种）</div>
      <div class="stat-val">${(c.total_closing_value || 0).toFixed(2)}</div>
      <div class="hint-text" style="margin-top:2px;">本期采购入库：${(c.total_in_amt || 0).toFixed(2)}</div>
    </div>
  `).join('');

  const tbody = document.getElementById('tbody-consumable-inv');
  tbody.innerHTML = cachedInventoryItems.map(it => {
    const priceBtn = it.price_range !== '暂无入库价'
      ? `<button class="price-link" onclick="openPriceDistModal(${it.item_id})">${it.price_range} 📊</button>`
      : `<span class="hint-text">暂无入库价</span>`;
    return `
      <tr data-closing-qty="${it.closing_base_qty}">
        <td>${it.seq}</td>
        <td>${it.category}</td>
        <td><strong>${it.item_name}</strong></td>
        <td>${it.base_unit}</td>
        <td>${it.pack_unit || '-'}</td>
        <td>${it.pack_spec}</td>
        <td class="col-qty"><strong>${it.opening_base_qty}</strong></td>
        <td class="col-qty">${it.opening_pack_qty}</td>
        <td class="col-qty" style="color:#059669;">${it.in_base_qty}</td>
        <td class="col-qty" style="color:#059669;">${it.in_pack_qty}</td>
        <td class="col-qty" style="color:#dc2626;">${it.out_base_qty}</td>
        <td class="col-qty" style="color:#dc2626;">${it.out_pack_qty}</td>
        <td class="col-qty" style="background:#eff6ff;"><strong>${it.closing_base_qty}</strong></td>
        <td class="col-qty" style="background:#eff6ff;">${it.closing_pack_qty}</td>
        <td class="col-money">${(it.moving_avg_price || 0).toFixed(2)}</td>
        <td class="col-money" style="background:#f0fdf4;color:#166534;"><strong>${(it.closing_value || 0).toFixed(2)}</strong></td>
        <td>${priceBtn}</td>
      </tr>`;
  }).join('');

  await loadConsumableLedger();
}

let cachedLedgerRows = [];
async function loadConsumableLedger() {
  const start = document.getElementById('inv-start-date').value;
  const end = document.getElementById('inv-end-date').value;
  const cat = document.getElementById('inv-category').value;
  const itemId = document.getElementById('inv-item-id').value;
  const txType = document.getElementById('ledger-type-filter').value;

  const qs = new URLSearchParams({ start_date: start, end_date: end, category: cat, item_id: itemId, tx_type: txType });
  const resp = await apiFetch(`/api/consumables/ledger?${qs.toString()}`);
  const res = await resp.json();
  if (!res.success) return;
  cachedLedgerRows = res.data || [];

  const tbody = document.getElementById('tbody-consumable-ledger');
  tbody.innerHTML = cachedLedgerRows.map(r => {
    const isIn = r.tx_type === 'IN';
    const amtText = isIn
      ? (r.total_amount > 0 ? r.total_amount.toFixed(2) : '0.00')
      : `<span style="color:#dc2626;">${(r.out_amount || 0).toFixed(2)}</span>`;
    const priceText = r.unit_price > 0 ? r.unit_price.toFixed(2) : '0.00';
    const editBtn = authState.role === 'admin'
      ? `<button class="btn btn-sm btn-outline" onclick="openEditLedgerModal(${r.id})">✏️ 修正</button>`
      : `<span class="hint-text">-</span>`;
    const dateWarnBadge = r.date_anomaly
      ? `<span class="badge badge-warn" title="${r.anomaly_msg}">⚠️ 日期倒挂</span>`
      : '';
    return `
      <tr class="${r.date_anomaly ? 'row-anomaly' : ''}">
        <td><code>${r.serial_no}</code></td>
        <td>${r.tx_date}${dateWarnBadge}</td>
        <td>${r.category}</td>
        <td><strong>${r.item_name}</strong></td>
        <td>${r.base_unit}</td>
        <td>${r.pack_unit || '-'}</td>
        <td>${r.pack_spec}</td>
        <td><span class="badge ${isIn ? 'badge-in' : 'badge-out'}">${isIn ? '入库' : '领用'}</span></td>
        <td class="col-qty">${r.before_qty}</td>
        <td class="col-qty"><strong>${r.base_qty}</strong></td>
        <td class="col-qty"><strong>${r.after_qty}</strong></td>
        <td class="col-qty">${r.pack_qty}</td>
        <td class="col-money">${amtText}</td>
        <td class="col-money">${priceText}</td>
        <td>${r.recipient || '-'}</td>
        <td>${r.remark || '-'}</td>
        <td class="no-print">${editBtn}</td>
      </tr>`;
  }).join('');
}

function openEditLedgerModal(id) {
  const row = cachedLedgerRows.find(x => x.id === id);
  if (!row) return;
  document.getElementById('edit-ledger-id').value = row.id;
  document.getElementById('edit-ledger-packsize').value = row.pack_size || 1;
  document.getElementById('edit-ledger-info').textContent = `正在修正流水：${row.serial_no} | ${row.item_name} (${row.tx_type === 'IN' ? '入库' : '领用'})`;
  document.getElementById('edit-ledger-serial').value = row.serial_no;
  document.getElementById('edit-ledger-date').value = row.tx_date;
  document.getElementById('edit-ledger-qty').value = row.base_qty;
  document.getElementById('edit-ledger-pack').value = row.pack_qty;
  document.getElementById('edit-ledger-amt').value = row.total_amount;
  document.getElementById('edit-ledger-recip').value = row.recipient;
  document.getElementById('edit-ledger-remark').value = row.remark;
  document.getElementById('edit-ledger-pass').value = '';

  if (row.tx_type === 'IN') {
    document.getElementById('edit-ledger-amt-group').classList.remove('hidden');
    document.getElementById('edit-ledger-recip-group').classList.add('hidden');
  } else {
    document.getElementById('edit-ledger-amt-group').classList.add('hidden');
    document.getElementById('edit-ledger-recip-group').classList.remove('hidden');
  }
  document.getElementById('modal-edit-ledger').classList.remove('hidden');
}

function onEditBaseQtyInput() {
  const pSize = parseInt(document.getElementById('edit-ledger-packsize').value, 10) || 1;
  const bQty = parseInt(document.getElementById('edit-ledger-qty').value, 10);
  if (!isNaN(bQty) && bQty > 0) {
    document.getElementById('edit-ledger-pack').value = Number((bQty / pSize).toFixed(2));
  }
}

function onEditPackQtyInput() {
  const pSize = parseInt(document.getElementById('edit-ledger-packsize').value, 10) || 1;
  const pQty = parseFloat(document.getElementById('edit-ledger-pack').value);
  if (!isNaN(pQty) && pQty > 0) {
    document.getElementById('edit-ledger-qty').value = Math.max(1, Math.round(pQty * pSize));
  }
}

async function submitEditLedger() {
  const id = document.getElementById('edit-ledger-id').value;
  const bQty = Number(document.getElementById('edit-ledger-qty').value);
  if (!Number.isInteger(bQty) || bQty <= 0) {
    alert('入库领用数量必须为正整数！');
    return;
  }
  const payload = {
    serial_no: document.getElementById('edit-ledger-serial').value.trim(),
    tx_date: document.getElementById('edit-ledger-date').value,
    base_qty: bQty,
    total_amount: parseFloat(document.getElementById('edit-ledger-amt').value) || 0,
    recipient: document.getElementById('edit-ledger-recip').value.trim(),
    remark: document.getElementById('edit-ledger-remark').value.trim(),
    admin_passcode: document.getElementById('edit-ledger-pass').value
  };
  const resp = await apiFetch(`/api/consumables/ledger/${id}`, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  alert(res.message);
  closeModal('modal-edit-ledger');
  await loadItems();
  await loadConsumableData();
}

// ==================== 4. 采购价格正态/频率分布直方图 ====================

async function openPriceDistModal(itemID) {
  const resp = await apiFetch(`/api/items/price-distribution?item_id=${itemID}`);
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  document.getElementById('pd-title').textContent = `📊 【${res.item_name}】采购价格分布与合理区间分析（单位：元）`;
  const body = document.getElementById('pd-body');

  if (!res.has_data) {
    body.innerHTML = `<div class="info-banner">该品种暂无带金额的采购入库记录。</div>`;
    document.getElementById('modal-price-dist').classList.remove('hidden');
    return;
  }

  const maxBatch = Math.max(...res.bins.map(b => b.batch_count), 1);

  const barsHtml = res.bins.map(b => {
    const barHeight = Math.round((b.batch_count / maxBatch) * 115);
    const bellBottom = Math.round(b.normal_pdf * 115);
    return `
      <div class="hist-col">
        <div class="hist-val">${b.batch_count}次 (${b.total_qty})</div>
        <div class="hist-bar-wrap">
          <div class="hist-bar" style="height:${barHeight}px;" title="${b.label}: 采购${b.batch_count}次"></div>
          <div class="hist-bell-dot" style="bottom:${bellBottom}px;" title="理论正态分布密度: ${(b.normal_pdf*100).toFixed(0)}%"></div>
        </div>
        <div class="hist-lbl">${b.label}</div>
      </div>`;
  }).join('');

  body.innerHTML = `
    <div class="summary-cards" style="grid-template-columns:repeat(4,1fr);">
      <div class="stat-card" style="border-left:4px solid #059669;">
        <div class="stat-title">✅ 建议合理采购单价区间 (μ±1σ)</div>
        <div class="stat-val" style="color:#059669;font-size:17px;">${res.reasonable_low.toFixed(2)} ~ ${res.reasonable_high.toFixed(2)}</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">加权平均采购价</div>
        <div class="stat-val" style="font-size:17px;">${res.weighted_price.toFixed(2)}</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">中位数 / 样本均值</div>
        <div class="stat-val" style="font-size:17px;">${res.median_price.toFixed(2)} / ${res.mean_price.toFixed(2)}</div>
      </div>
      <div class="stat-card">
        <div class="stat-title">历史最低 ~ 最高 (样本数)</div>
        <div class="stat-val" style="font-size:16px;">${res.min_price.toFixed(2)} ~ ${res.max_price.toFixed(2)} (${res.batch_total}次)</div>
      </div>
    </div>
    <div class="hint-text">蓝色柱体表示各价格区间实际采购频次，橙色圆点表示理论正态分布钟形趋势：</div>
    <div class="hist-chart">${barsHtml}</div>
    <div class="table-responsive" style="max-height:200px;">
      <table class="data-table">
        <thead><tr><th>采购日期</th><th class="col-qty">入库数量 (${res.base_unit})</th><th class="col-money">采购总金额</th><th class="col-money">折算单价</th></tr></thead>
        <tbody>
          ${res.records.map(rec => `<tr><td>${rec.date}</td><td class="col-qty">${rec.qty}</td><td class="col-money">${rec.total_amount.toFixed(2)}</td><td class="col-money"><strong>${rec.unit_price.toFixed(2)}</strong></td></tr>`).join('')}
        </tbody>
      </table>
    </div>
  `;
  document.getElementById('modal-price-dist').classList.remove('hidden');
}

// ==================== 5. 复用品（类资产全生命周期 + 管理员补录修改 + 预计处置日期 + 履历打印导出） ====================

function openReusableInModal() {
  const reusables = allItems.filter(i => i.nature === 'reusable');
  document.getElementById('ra-in-item-id').innerHTML = reusables.map(i => `<option value="${i.id}">[${i.category}] ${i.name}</option>`).join('');
  document.getElementById('ra-in-subname').value = '';
  document.getElementById('ra-in-date').value = todayStr();
  document.getElementById('ra-in-qty').value = '1';
  document.getElementById('ra-in-amount').value = '0';
  document.getElementById('ra-in-years').value = '10';
  document.getElementById('ra-in-file').value = '';
  document.getElementById('ra-in-remark').value = '';
  document.getElementById('modal-reusable-in').classList.remove('hidden');
}

async function submitReusableIn() {
  const fileInput = document.getElementById('ra-in-file');
  if (!fileInput.files || fileInput.files.length === 0) {
    alert('请上传复用品实物照片附件！');
    return;
  }
  const fd = new FormData();
  fd.append('item_id', document.getElementById('ra-in-item-id').value);
  fd.append('sub_name', document.getElementById('ra-in-subname').value.trim());
  fd.append('purchase_date', document.getElementById('ra-in-date').value);
  fd.append('quantity', document.getElementById('ra-in-qty').value);
  fd.append('total_amount', document.getElementById('ra-in-amount').value);
  fd.append('expected_years', document.getElementById('ra-in-years').value);
  fd.append('remark', document.getElementById('ra-in-remark').value.trim());
  fd.append('image_file', fileInput.files[0]);

  const resp = await apiFetch('/api/reusables/assets', { method: 'POST', body: fd });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  alert(res.message);
  closeModal('modal-reusable-in');
  await loadItems();
  await loadReusableAssets();
}

function openReusableEditModal(assetID) {
  const assetObj = cachedReusableAssets.find(x => x.id === assetID);
  if (!assetObj) return;
  const reusables = allItems.filter(i => i.nature === 'reusable');
  document.getElementById('ra-edit-id').value = assetObj.id;
  document.getElementById('ra-edit-title').textContent = `✏️ 补录 / 修改类资产档案 (${assetObj.asset_no})`;
  document.getElementById('ra-edit-item-id').innerHTML = reusables.map(i =>
    `<option value="${i.id}" ${i.id === assetObj.item_id ? 'selected' : ''}>[${i.category}] ${i.name}</option>`
  ).join('');
  document.getElementById('ra-edit-subname').value = assetObj.sub_name || '';
  document.getElementById('ra-edit-date').value = assetObj.purchase_date || todayStr();
  document.getElementById('ra-edit-price').value = assetObj.unit_price || 0;
  document.getElementById('ra-edit-years').value = assetObj.expected_years || 10;
  document.getElementById('ra-edit-file').value = '';
  document.getElementById('ra-edit-remark').value = assetObj.remark || '';
  document.getElementById('ra-edit-pass').value = '';
  document.getElementById('modal-reusable-edit').classList.remove('hidden');
}

async function submitReusableEdit() {
  const id = document.getElementById('ra-edit-id').value;
  const pass = document.getElementById('ra-edit-pass').value.trim();
  if (!pass) {
    alert('修改或补录类资产档案需要输入管理员口令！');
    return;
  }
  const fd = new FormData();
  fd.append('item_id', document.getElementById('ra-edit-item-id').value);
  fd.append('sub_name', document.getElementById('ra-edit-subname').value.trim());
  fd.append('purchase_date', document.getElementById('ra-edit-date').value);
  fd.append('unit_price', document.getElementById('ra-edit-price').value);
  fd.append('expected_years', document.getElementById('ra-edit-years').value);
  fd.append('remark', document.getElementById('ra-edit-remark').value.trim());
  fd.append('admin_passcode', pass);

  const fileInput = document.getElementById('ra-edit-file');
  if (fileInput.files && fileInput.files.length > 0) {
    fd.append('image_file', fileInput.files[0]);
  }

  const resp = await apiFetch(`/api/reusables/assets/${id}`, { method: 'PUT', body: fd });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  alert(res.message);
  closeModal('modal-reusable-edit');
  await loadItems();
  await loadReusableAssets();
}

async function loadReusableAssets() {
  const cat = document.getElementById('ra-category').value;
  const st = document.getElementById('ra-status').value;
  const kw = document.getElementById('ra-kw').value.trim();

  const qs = new URLSearchParams({ category: cat, status: st, kw: kw });
  const resp = await apiFetch(`/api/reusables/assets?${qs.toString()}`);
  const res = await resp.json();
  if (!res.success) return;
  cachedReusableAssets = res.data || [];

  const s = res.summary || {};
  document.getElementById('reusable-summary-cards').innerHTML = `
    <div class="stat-card"><div class="stat-title">资产总数</div><div class="stat-val">${s.total_count || 0}</div></div>
    <div class="stat-card"><div class="stat-title">在用领出</div><div class="stat-val" style="color:#d97706;">${s.in_use_count || 0}</div></div>
    <div class="stat-card"><div class="stat-title">闲置在库</div><div class="stat-val" style="color:#0284c7;">${s.idle_count || 0}</div></div>
    <div class="stat-card"><div class="stat-title">已报废</div><div class="stat-val" style="color:#64748b;">${s.scrap_count || 0}</div></div>
    <div class="stat-card"><div class="stat-title">在库/在用资产原值 (元)</div><div class="stat-val" style="color:#059669;">${(s.active_value || 0).toFixed(2)}</div></div>
  `;

  const tbody = document.getElementById('tbody-reusable-assets');
  tbody.innerHTML = cachedReusableAssets.map(a => {
    let badgeCls = 'badge-idle';
    if (a.status === '在用') badgeCls = 'badge-use';
    if (a.status === '报废') badgeCls = 'badge-scrap';

    const imgHtml = a.image_path
      ? `<a href="/attachments/${a.image_path}" target="_blank"><img src="/attachments/${a.image_path}" class="thumb-img" alt="实物图" /></a>`
      : '-';

    let ops = `<button class="btn btn-sm btn-outline" onclick="openAssetLogsModal(${a.id})">📜 履历</button> `;
    if (authState.role === 'admin') {
      ops += `<button class="btn btn-sm btn-outline" onclick="openReusableEditModal(${a.id})">✏️ 编辑</button> `;
    }
    if (a.status === '闲置') {
      ops += `<button class="btn btn-sm btn-primary" onclick="openAssetTransitionModal(${a.id}, 'ASSIGN', '${a.asset_no}', '')">📤 首次领用</button> `;
      ops += `<button class="btn btn-sm btn-danger" onclick="openAssetTransitionModal(${a.id}, 'SCRAP', '${a.asset_no}', '')">🗑️ 报废</button>`;
    } else if (a.status === '在用') {
      ops += `<button class="btn btn-sm btn-outline" onclick="openAssetTransitionModal(${a.id}, 'TRANSFER', '${a.asset_no}', '${a.current_user}')">🔄 变更使用人</button> `;
      ops += `<button class="btn btn-sm btn-danger" onclick="openAssetTransitionModal(${a.id}, 'SCRAP', '${a.asset_no}', '${a.current_user}')">🗑️ 报废</button>`;
    } else if (a.status === '报废') {
      if (a.scrap_file_path) {
        ops += `<a class="btn btn-sm btn-outline" href="/attachments/${a.scrap_file_path}" target="_blank">📎 报废凭证</a> `;
      }
      if (authState.role === 'admin') {
        ops += `<button class="btn btn-sm btn-outline" style="color:#dc2626;border-color:#fca5a5;" onclick="openAssetTransitionModal(${a.id}, 'UNSCRAP', '${a.asset_no}', '')">↩️ 取消报废</button>`;
      }
    }

    return `
      <tr>
        <td><code>${a.asset_no}</code></td>
        <td>${a.category}</td>
        <td><strong>${a.item_name}</strong></td>
        <td>${a.sub_name || '-'}</td>
        <td class="no-print">${imgHtml}</td>
        <td>${a.purchase_date}</td>
        <td class="col-money">${a.unit_price.toFixed(2)}</td>
        <td class="col-qty">${a.expected_years}</td>
        <td>${a.disposal_date}</td>
        <td><span class="badge ${badgeCls}">${a.status}</span></td>
        <td><strong>${a.current_user || '-'}</strong></td>
        <td class="no-print" style="white-space:nowrap;">${ops}</td>
      </tr>`;
  }).join('');
}

function openAssetTransitionModal(assetID, action, assetNo, curUser) {
  document.getElementById('ra-trans-asset-id').value = assetID;
  document.getElementById('ra-trans-action').value = action;
  document.getElementById('ra-trans-date').value = todayStr();
  document.getElementById('ra-trans-user').value = '';
  document.getElementById('ra-trans-file').value = '';
  document.getElementById('ra-trans-pass').value = '';
  document.getElementById('ra-trans-remark').value = '';

  const userGroup = document.getElementById('ra-trans-user-group');
  const scrapGroup = document.getElementById('ra-trans-scrap-group');
  const passGroup = document.getElementById('ra-trans-pass-group');
  userGroup.classList.add('hidden');
  scrapGroup.classList.add('hidden');
  passGroup.classList.add('hidden');

  if (action === 'ASSIGN') {
    document.getElementById('ra-trans-title').textContent = `📤 首次领用登记 (${assetNo})`;
    document.getElementById('ra-trans-banner').textContent = `资产 ${assetNo} 当前处于【闲置】状态，分配后将变为【在用】并锁定防重领。`;
    userGroup.classList.remove('hidden');
  } else if (action === 'TRANSFER') {
    document.getElementById('ra-trans-title').textContent = `🔄 变更使用人 (${assetNo})`;
    document.getElementById('ra-trans-banner').textContent = `当前使用人为【${curUser}】，请输入交接后的新使用人。`;
    userGroup.classList.remove('hidden');
  } else if (action === 'SCRAP') {
    document.getElementById('ra-trans-title').textContent = `🗑️ 登记资产报废 (${assetNo})`;
    document.getElementById('ra-trans-banner').textContent = `登记报废必须上传对应的报废审批单或现场损坏照片作为凭证！`;
    scrapGroup.classList.remove('hidden');
  } else if (action === 'UNSCRAP') {
    document.getElementById('ra-trans-title').textContent = `↩️ 管理员特权取消报废 (${assetNo})`;
    document.getElementById('ra-trans-banner').textContent = `取消报废后，资产 ${assetNo} 将恢复为【闲置】可用状态并写入全生命周期履历与系统审计日志。`;
    passGroup.classList.remove('hidden');
  }
  document.getElementById('modal-reusable-transition').classList.remove('hidden');
}

async function submitReusableTransition() {
  const assetID = document.getElementById('ra-trans-asset-id').value;
  const action = document.getElementById('ra-trans-action').value;
  const fd = new FormData();
  fd.append('action', action);
  fd.append('action_date', document.getElementById('ra-trans-date').value);
  fd.append('user', document.getElementById('ra-trans-user').value.trim());
  fd.append('remark', document.getElementById('ra-trans-remark').value.trim());
  fd.append('admin_passcode', document.getElementById('ra-trans-pass').value.trim());

  if (action === 'SCRAP') {
    const f = document.getElementById('ra-trans-file');
    if (!f.files || f.files.length === 0) {
      alert('报废登记必须上传凭证资料！');
      return;
    }
    fd.append('scrap_file', f.files[0]);
  }

  const resp = await apiFetch(`/api/reusables/assets/${assetID}/transition`, { method: 'POST', body: fd });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  closeModal('modal-reusable-transition');
  await loadRecipients();
  await loadReusableAssets();
}

async function openAssetLogsModal(assetID) {
  const assetObj = cachedReusableAssets.find(x => x.id === assetID);
  currentInspectingAsset = assetObj || { id: assetID, asset_no: '', item_name: '' };

  document.getElementById('ra-logs-title').textContent = `📜 资产 ${currentInspectingAsset.asset_no} 全生命周期流转履历`;
  document.getElementById('ra-logs-meta').innerHTML = assetObj
    ? `<strong>标准品名：</strong>${assetObj.item_name}（${assetObj.sub_name || '-'}） &nbsp;|&nbsp; <strong>购置日期：</strong>${assetObj.purchase_date} &nbsp;|&nbsp; <strong>购置单价：</strong>${assetObj.unit_price.toFixed(2)} 元 &nbsp;|&nbsp; <strong>预计年限：</strong>${assetObj.expected_years} 年 &nbsp;|&nbsp; <strong>预计处置日期：</strong>${assetObj.disposal_date} &nbsp;|&nbsp; <strong>当前状态：</strong>${assetObj.status}`
    : '';

  const resp = await apiFetch(`/api/reusables/assets/${assetID}/logs`);
  const res = await resp.json();
  const tbody = document.getElementById('tbody-ra-single-logs');
  tbody.innerHTML = (res.data || []).map(l => `
    <tr>
      <td><code>${l.log_no}</code></td>
      <td>${l.action_date}</td>
      <td><strong>${l.action_type}</strong></td>
      <td>${l.operator_or_user || '-'}</td>
      <td>${l.detail}</td>
      <td class="no-print">${l.attachment_path ? `<a href="/attachments/${l.attachment_path}" target="_blank">查看附件</a>` : '-'}</td>
    </tr>
  `).join('');
  document.getElementById('modal-reusable-logs').classList.remove('hidden');
}

function printSingleAssetLogs() {
  openPrintPreview('reusable_single_logs');
}

function exportSingleAssetLogs() {
  if (!currentInspectingAsset || !currentInspectingAsset.id) return;
  const qs = new URLSearchParams({
    mode: 'reusable_single_logs',
    asset_id: currentInspectingAsset.id,
    org_name: authState.org_name || '综合行政部'
  });
  if (authState.token) qs.set('token', authState.token);
  window.open(`/api/export/excel?${qs.toString()}`, '_blank');
}

// ==================== 6. 月度原始手写登记附件 ====================

function openMonthlyUploadModal() {
  const ym = new Date().toISOString().slice(0, 7);
  document.getElementById('ma-up-ym').value = ym;
  document.getElementById('ma-up-title').value = `${ym}_办公用品月度手写领用登记表`;
  document.getElementById('ma-up-file').value = '';
  document.getElementById('ma-up-remark').value = '';
  document.getElementById('modal-monthly-upload').classList.remove('hidden');
}

async function submitMonthlyUpload() {
  const f = document.getElementById('ma-up-file');
  if (!f.files || f.files.length === 0) {
    alert('请选择要上传的扫描件或照片！');
    return;
  }
  const fd = new FormData();
  fd.append('year_month', document.getElementById('ma-up-ym').value);
  fd.append('title', document.getElementById('ma-up-title').value.trim());
  fd.append('remark', document.getElementById('ma-up-remark').value.trim());
  fd.append('file', f.files[0]);

  const resp = await apiFetch('/api/monthly-attachments', { method: 'POST', body: fd });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  closeModal('modal-monthly-upload');
  await loadMonthlyAttachments();
}

async function loadMonthlyAttachments() {
  const ym = document.getElementById('ma-filter-ym').value;
  const resp = await apiFetch(`/api/monthly-attachments?year_month=${ym}`);
  const res = await resp.json();
  const tbody = document.getElementById('tbody-monthly-attachments');
  tbody.innerHTML = (res.data || []).map(m => {
    const delBtn = authState.role === 'admin'
      ? `<button class="btn btn-sm btn-danger" onclick="deleteMonthlyAttachment(${m.id})">🗑️ 删除</button>`
      : '';
    return `
      <tr>
        <td><strong>${m.year_month}</strong></td>
        <td>${m.title}</td>
        <td>${m.original_name}</td>
        <td>${m.remark || '-'}</td>
        <td>${m.uploaded_at}</td>
        <td class="no-print" style="white-space:nowrap;">
          <a class="btn btn-sm btn-outline" href="/attachments/${m.file_path}" target="_blank">👁️ 预览</a>
          <a class="btn btn-sm btn-outline" href="/attachments/${m.file_path}?download=1">📥 下载</a>
          ${delBtn}
        </td>
      </tr>`;
  }).join('');
}

async function deleteMonthlyAttachment(id) {
  const pass = prompt('删除月度原始凭证属于高危操作，请输入管理员口令确认：');
  if (!pass) return;
  const resp = await apiFetch(`/api/monthly-attachments/${id}?admin_passcode=${encodeURIComponent(pass)}`, { method: 'DELETE' });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  await loadMonthlyAttachments();
}

// ==================== 7. 年度结转、撤销结转、设置与审计日志 ====================

async function openCarryoverModal() {
  document.getElementById('co-year').value = new Date().getFullYear();
  document.getElementById('co-pass').value = '';
  const resp = await apiFetch('/api/carryover');
  const res = await resp.json();
  const list = res.data || [];
  document.getElementById('co-history-list').innerHTML = list.length
    ? `<div class="hint-text">已执行结转记录：${list.map(x => `${x.carry_year}年度 (${x.created_at})`).join('；')}</div>`
    : `<div class="hint-text">暂无历史年度结转记录。</div>`;
  document.getElementById('modal-carryover').classList.remove('hidden');
}

async function submitCarryover() {
  const payload = {
    carry_year: parseInt(document.getElementById('co-year').value, 10),
    admin_passcode: document.getElementById('co-pass').value
  };
  const resp = await apiFetch('/api/carryover', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  alert(res.message);
  closeModal('modal-carryover');
  await loadConsumableData();
}

async function submitCancelCarryover() {
  const year = parseInt(document.getElementById('co-year').value, 10);
  const pass = document.getElementById('co-pass').value;
  if (!pass) {
    alert('撤销年度结转请输入管理员最高权限口令！');
    return;
  }
  if (!confirm(`确定要撤销 ${year} 年度的结转锁定快照吗？`)) return;
  const resp = await apiFetch(`/api/carryover?carry_year=${year}&admin_passcode=${encodeURIComponent(pass)}`, {
    method: 'DELETE'
  });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  alert(res.message);
  closeModal('modal-carryover');
  await loadItems();
  await loadConsumableData();
}

function openSettingsModal() {
  document.getElementById('set-org-name').value = authState.org_name || '综合行政部';
  document.getElementById('set-port').value = authState.port || 8088;
  document.getElementById('set-allow-lan').value = String(authState.allow_lan);
  document.getElementById('set-admin-pass').value = authState.admin_passcode || '';
  document.getElementById('set-entry-pass').value = authState.entry_passcode || '';
  document.getElementById('lan-share-box').innerHTML =
    `🌐 <strong>内网共享网址：</strong> ${(authState.lan_urls || []).join(' 或 ') || '未检测到局域网网卡'}<br/>` +
    `• 将 <strong>内网登记口令</strong> 发给同事，他们只能登记和查看，无法篡改或删除历史账目。`;
  document.getElementById('modal-settings').classList.remove('hidden');
}

async function submitSettings() {
  const payload = {
    org_name: document.getElementById('set-org-name').value.trim(),
    port: parseInt(document.getElementById('set-port').value, 10),
    allow_lan: document.getElementById('set-allow-lan').value === 'true',
    admin_passcode: document.getElementById('set-admin-pass').value.trim(),
    entry_passcode: document.getElementById('set-entry-pass').value.trim()
  };
  const resp = await apiFetch('/api/settings', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload)
  });
  const res = await resp.json();
  if (!res.success) {
    alert(res.message);
    return;
  }
  closeModal('modal-settings');
  if (res.need_restart) {
    alert(`端口已切换为 ${res.new_port}，即将自动跳转至新地址！`);
    setTimeout(() => {
      window.location.href = `http://${window.location.hostname}:${res.new_port}`;
    }, 800);
  } else {
    alert('系统设置已保存！');
    await checkAuthAndInit();
  }
}

async function loadAuditLogs() {
  const resp = await apiFetch('/api/audit-logs');
  const res = await resp.json();
  document.getElementById('tbody-audit-logs').innerHTML = (res.data || []).map(l => `
    <tr>
      <td>${l.created_at}</td>
      <td><strong>${l.action}</strong></td>
      <td>${l.target_info}</td>
      <td>${l.detail}</td>
    </tr>
  `).join('');
}

// ==================== 8. 自定义打印（右上角金额单位：元 + 所有列缩放到一页 + 隐藏0结存选项） ====================

function openPrintPreview(mode) {
  currentPrintMode = mode;
  const start = document.getElementById('inv-start-date').value;
  const end = document.getElementById('inv-end-date').value;

  let defaultTitle = '办公消耗品期初/期末盘点与移动加权在库价值报表';
  let subRightMeta = `统计区间：${start} 至 ${end} &nbsp;&nbsp;|&nbsp;&nbsp; <strong>金额单位：元</strong>`;
  let tableHtml = document.getElementById('table-consumable-inv').outerHTML;
  let cardsHtml = document.getElementById('consumable-cat-cards').outerHTML;
  let hasCardsOption = true;
  let hasZeroFilterOption = true;

  if (mode === 'consumable_ledger') {
    defaultTitle = '办公消耗品入库与领用接续流水账';
    tableHtml = document.getElementById('table-consumable-ledger').outerHTML;
    cardsHtml = '';
    hasCardsOption = false;
    hasZeroFilterOption = false;
  } else if (mode === 'reusable_assets') {
    defaultTitle = '企业复用品（类资产）独立编码盘点台账';
    subRightMeta = `盘点日期：${todayStr()} &nbsp;&nbsp;|&nbsp;&nbsp; <strong>金额单位：元</strong>`;
    tableHtml = document.getElementById('table-reusable-assets').outerHTML;
    cardsHtml = document.getElementById('reusable-summary-cards').outerHTML;
    hasZeroFilterOption = false;
  } else if (mode === 'reusable_single_logs') {
    const aNo = currentInspectingAsset ? currentInspectingAsset.asset_no : '';
    const aName = currentInspectingAsset ? currentInspectingAsset.item_name : '';
    defaultTitle = `复用品（类资产）全生命周期履历表 - ${aNo} (${aName})`;
    subRightMeta = `打印日期：${todayStr()} &nbsp;&nbsp;|&nbsp;&nbsp; <strong>金额单位：元</strong>`;
    const metaBanner = document.getElementById('ra-logs-meta').innerHTML;
    tableHtml = `<div style="margin-bottom:10px;font-size:12px;color:#1e293b;border:1px solid #cbd5e1;padding:8px;background:#f8fafc;">${metaBanner}</div>` +
      document.getElementById('table-single-asset-logs').outerHTML;
    cardsHtml = '';
    hasCardsOption = false;
    hasZeroFilterOption = false;
  }

  document.getElementById('print-org-input').value = authState.org_name || '综合行政部';
  document.getElementById('print-title-input').value = defaultTitle;
  document.getElementById('print-include-cards').checked = false;

  const mainHideZero = document.getElementById('inv-hide-zero').checked;
  document.getElementById('print-hide-zero').checked = mainHideZero;

  const toggleWrap = document.getElementById('print-cards-toggle-wrap');
  if (hasCardsOption) {
    toggleWrap.classList.remove('hidden');
  } else {
    toggleWrap.classList.add('hidden');
  }

  const zeroWrap = document.getElementById('print-hide-zero-wrap');
  if (hasZeroFilterOption) {
    zeroWrap.classList.remove('hidden');
  } else {
    zeroWrap.classList.add('hidden');
  }

  const area = document.getElementById('print-preview-area');
  area.innerHTML = `
    <div id="print-sheet-wrapper" style="transform-origin:top left;">
      <div style="display:flex;justify-content:space-between;align-items:flex-end;margin-bottom:6px;font-size:13px;color:#334155;">
        <div><strong>单位/部门：</strong><span id="print-org-display"></span></div>
        <div>${subRightMeta}</div>
      </div>
      <div style="text-align:center;margin-bottom:12px;">
        <h2 id="print-title-display" style="font-size:19px;margin-bottom:4px;"></h2>
      </div>
      <div id="print-cards-container" class="hidden" style="margin-bottom:12px;">${cardsHtml}</div>
      ${tableHtml}
      <div style="margin-top:24px;display:flex;justify-content:space-between;font-size:12px;color:#475569;">
        <span>制表人：_______________</span>
        <span>复核人：_______________</span>
        <span>打印时间：${todayStr()}</span>
      </div>
    </div>
  `;

  area.querySelectorAll('.no-print').forEach(el => el.remove());
  area.querySelectorAll('table').forEach(tbl => {
    tbl.style.width = '100%';
    tbl.style.borderCollapse = 'collapse';
    tbl.style.fontSize = '11px';
    tbl.querySelectorAll('th').forEach(th => {
      th.style.border = '1px solid #334155';
      th.style.padding = '5px 5px';
      th.style.textAlign = 'center';
      th.style.whiteSpace = 'normal';
      th.style.wordBreak = 'break-word';
    });
    tbl.querySelectorAll('td').forEach(td => {
      td.style.border = '1px solid #334155';
      td.style.padding = '5px 5px';
      td.style.whiteSpace = 'normal';
      td.style.wordBreak = 'break-word';
      td.style.textAlign = td.classList.contains('col-money') ? 'right' : 'center';
    });
  });

  refreshPrintHeader();
  refreshPrintCards();
  if (hasZeroFilterOption) {
    refreshPrintHideZero();
  }
  document.getElementById('modal-print-preview').classList.remove('hidden');
  setTimeout(applyPrintLayout, 60);
}

function refreshPrintHideZero() {
  const hideZero = document.getElementById('print-hide-zero').checked;
  const tbl = document.querySelector('#print-preview-area #table-consumable-inv');
  if (!tbl) return;
  let seq = 0;
  tbl.querySelectorAll('tbody tr').forEach(tr => {
    const closingQty = parseInt(tr.getAttribute('data-closing-qty') || '0', 10);
    if (hideZero && closingQty === 0) {
      tr.style.display = 'none';
    } else {
      tr.style.display = '';
      seq++;
      const firstCell = tr.querySelector('td');
      if (firstCell) firstCell.textContent = String(seq);
    }
  });
  applyPrintLayout();
}

function refreshPrintHeader() {
  const orgEl = document.getElementById('print-org-display');
  const titleEl = document.getElementById('print-title-display');
  if (orgEl) orgEl.textContent = document.getElementById('print-org-input').value.trim() || '综合行政部';
  if (titleEl) titleEl.textContent = document.getElementById('print-title-input').value.trim();
}

function refreshPrintCards() {
  const checked = document.getElementById('print-include-cards').checked;
  const box = document.getElementById('print-cards-container');
  if (!box) return;
  if (checked) {
    box.classList.remove('hidden');
  } else {
    box.classList.add('hidden');
  }
}

function applyPrintLayout() {
  const orient = document.getElementById('print-orient').value;
  const scaleMode = document.getElementById('print-scale-mode').value;
  const wrapper = document.getElementById('print-sheet-wrapper');
  if (!wrapper) return;

  let styleTag = document.getElementById('dynamic-print-page-style');
  if (!styleTag) {
    styleTag = document.createElement('style');
    styleTag.id = 'dynamic-print-page-style';
    document.head.appendChild(styleTag);
  }
  styleTag.textContent = `@media print { @page { size: A4 ${orient}; margin: 10mm; } }`;

  wrapper.style.zoom = '1';
  wrapper.style.width = 'max-content';
  const contentWidth = wrapper.scrollWidth;
  const pageTargetWidth = orient === 'landscape' ? 1040 : 710;

  let finalZoom = 1;
  if (scaleMode === 'fit') {
    if (contentWidth > pageTargetWidth) {
      finalZoom = Math.max(0.42, Number((pageTargetWidth / contentWidth).toFixed(3)));
    }
  } else {
    finalZoom = parseFloat(scaleMode) || 1;
  }

  wrapper.style.width = contentWidth > pageTargetWidth ? `${contentWidth}px` : '100%';
  wrapper.style.zoom = String(finalZoom);
}

function exportExcel(mode) {
  const start = document.getElementById('inv-start-date').value;
  const end = document.getElementById('inv-end-date').value;
  const orgName = authState.org_name || '综合行政部';
  const hideZero = document.getElementById('inv-hide-zero').checked ? '1' : '0';
  const qs = new URLSearchParams({
    mode: mode,
    start_date: start,
    end_date: end,
    org_name: orgName,
    hide_zero: hideZero
  });
  if (authState.token) {
    qs.set('token', authState.token);
  }
  window.open(`/api/export/excel?${qs.toString()}`, '_blank');
}
