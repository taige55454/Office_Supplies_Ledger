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
    const spec = it.pack_unit ? `1${it.pack_unit} = ${it.pack_size}${it.base_unit}` : `1${it.base_unit}`;
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
