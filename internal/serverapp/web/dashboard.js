import { $, copyText, escapeHTML, redirectToLogin, showToast, statusText } from './ui.js';

// CSRF 令牌由服务端模板注入。所有改变服务端状态的请求都必须携带该值，
// 防止其他站点借用管理员浏览器中的会话发起跨站请求。
const csrf = document.querySelector('meta[name="csrf-token"]').content;
// 保存最近一次快照；Client 选择和任务筛选都从快照派生，避免刷新时丢失本地操作。
let clients = [];
let tasks = [];
let refreshInFlight = null;
let clientsLoadInFlight = null;

// api 统一处理管理端 JSON API 的请求头、会话失效和错误格式。
const api = async (url, options = {}) => {
  const headers = { ...(options.headers || {}) };
  if (options.body) headers['Content-Type'] = 'application/json';
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch(url, { ...options, headers });
  if (redirectToLogin(response)) throw new Error('登录已失效');
  let data = null;
  try { data = response.status === 204 ? null : await response.json(); } catch (_) { /* 非 JSON 错误由下方统一处理。 */ }
  if (!response.ok) throw new Error(data?.error || `请求失败（${response.status}）`);
  return data;
};

const dateText = value => value ? new Date(value).toLocaleString() : '—';

// 创建凭据时根据当前管理页面入口生成对应 WebSocket 地址。Token 放进临时环境变量，
// 避免直接作为进程参数出现；该命令只存在于一次性创建响应和当前对话框内。
function windowsClientCommand(token) {
  const scheme = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
  const serverURL = `${scheme}//${window.location.host}/ws/client`;
  return `$env:SMALUX_CLIENT_TOKEN='${String(token).replaceAll("'", "''")}'\n.\\smalux-client-windows-amd64.exe -server '${serverURL}'`;
}

function setSyncStatus(state, text) {
  const status = $('#sync-status');
  if (!status) return;
  status.className = `sync-status ${state}`;
  status.textContent = text;
}

function selectedClientIDs() {
  return new Set([...document.querySelectorAll('#client-options input[name="client_id"]:checked:not(:disabled)')].map(input => input.value));
}

function updateSelectionSummary() {
  const selected = selectedClientIDs();
  const summary = $('#selected-client-count');
  if (summary) summary.textContent = `${selected.size} 个已选择`;
  const submit = $('#task-form button[type="submit"]');
  if (submit && !submit.disabled) submit.title = selected.size ? '' : '请至少选择一个 Client';
}

// Client 选项在轮询时重新绘制，但始终保留管理员已经勾选的 ID；首次加载自动选中在线节点。
function renderClientOptions(previousSelection, firstRender) {
  const available = clients.filter(item => item.enabled);
  if (!available.length) {
    $('#client-options').innerHTML = '<span class="muted">暂无可用 Client</span>';
    updateSelectionSummary();
    return;
  }
  const selected = new Set(previousSelection);
  if (firstRender && selected.size === 0) available.filter(item => item.online && item.manageable).forEach(item => selected.add(item.id));
  $('#client-options').innerHTML = available.map(item => `
    <label class="check-option ${item.online ? '' : 'offline'} ${item.manageable ? '' : 'readonly'}">
      <input type="checkbox" name="client_id" value="${escapeHTML(item.id)}" ${selected.has(item.id) && item.manageable ? 'checked' : ''} ${item.manageable ? '' : 'disabled'}>
      <span>${escapeHTML(item.name)}</span><span class="client-state ${item.online ? 'online' : ''}">${item.online ? '在线' : '离线'}${item.manageable ? '' : ' · 只读'}</span>
    </label>`).join('');
  updateSelectionSummary();
}

// 拉取 Client 注册信息与实时在线状态，并同步刷新概览、表格和任务目标选择器。
async function loadClients({force = false} = {}) {
  // 刷新、创建和吊销共用同一个请求；变更操作等待旧快照结束后再发起一轮，
  // 防止旧响应在新 Client 已创建时回写表格。
  if (clientsLoadInFlight) {
    if (!force) return clientsLoadInFlight;
    const activeRequest = clientsLoadInFlight;
    try { await activeRequest; } catch (_) { /* 变更后的强制刷新仍应重新尝试。 */ }
    if (clientsLoadInFlight === activeRequest) clientsLoadInFlight = null;
    // 递归进入普通路径；多个同时等待的 force 调用会共享接下来这一轮请求。
    return loadClients();
  }
  const request = (async () => {
    const firstRender = !$('#client-options input[name="client_id"]');
    const nextClients = await api('/api/clients');
    clients = Array.isArray(nextClients) ? nextClients : [];
    // 在请求期间管理员可能已经勾选或清空节点；响应返回后再读取 DOM，避免覆盖刚完成的操作。
    const previousSelection = selectedClientIDs();
    $('#online-count').textContent = clients.filter(item => item.online).length;
    $('#client-count').textContent = clients.length;
    $('#clients-body').innerHTML = clients.length ? clients.map(item => `
      <tr>
        <td><span class="status ${item.online ? 'online' : ''}">${item.online ? '在线' : '离线'}</span></td>
        <td>${item.manageable ? `<button class="client-edit-trigger edit-client" data-id="${escapeHTML(item.id)}" title="编辑名称和标签" type="button"><strong>${escapeHTML(item.name)}</strong><span>编辑</span></button>` : `<strong>${escapeHTML(item.name)}</strong>`}</td>
        <td>${escapeHTML([item.os, item.arch].filter(Boolean).join(' / ') || '—')}</td>
        <td class="mono">${escapeHTML(item.version || '—')}</td>
        <td>${Object.entries(item.labels || {}).map(([key,value]) => `<span class="tag">${escapeHTML(key)}=${escapeHTML(value)}</span>`).join('') || '—'}</td>
        <td>${dateText(item.last_seen)}</td>
        <td><div class="row-actions">${item.manageable ? `<button class="quiet edit-client" data-id="${escapeHTML(item.id)}" title="编辑名称和标签" type="button">编辑</button><button class="danger revoke-client" data-id="${escapeHTML(item.id)}" title="永久吊销 Token" type="button">吊销</button>` : '<span class="muted">只读</span>'}</div></td>
      </tr>`).join('') : '<tr><td colspan="7" class="empty">暂无 Client</td></tr>';
    renderClientOptions(previousSelection, firstRender);
    return clients;
  })();
  clientsLoadInFlight = request;
  try {
    return await request;
  } finally {
    if (clientsLoadInFlight === request) clientsLoadInFlight = null;
  }
}

function renderTasks() {
  const filter = $('#task-filter')?.value || 'all';
  const search = ($('#task-search')?.value || '').trim().toLowerCase();
  const visible = tasks.filter(item => {
    const matchesFilter = filter === 'all' || (filter === 'active' ? ['queued', 'running'].includes(item.status) : item.status === filter);
    const matchesSearch = !search || String(item.id || '').toLowerCase().includes(search);
    return matchesFilter && matchesSearch;
  });
  $('#tasks-body').innerHTML = visible.length ? visible.map(item => `
    <tr>
      <td>${dateText(item.created_at)}</td>
      <td><span class="status ${escapeHTML(item.status)}">${escapeHTML(statusText(item.status))}</span></td>
      <td>${item.proxy_count}</td><td>${item.client_count}</td><td>Top ${item.top_n} / ${item.candidate_count}</td><td>${item.threads}</td>
      <td><a class="quiet button-link" href="/tasks/${escapeHTML(item.id)}">查看</a></td>
    </tr>`).join('') : `<tr><td colspan="7" class="empty">${tasks.length ? '没有匹配的任务' : '暂无任务'}</td></tr>`;
}

// 任务筛选只在浏览器内完成，不额外请求数据库；历史快照仍限制为服务端最近 100 条。
async function loadTasks() {
  const nextTasks = await api('/api/tasks');
  tasks = Array.isArray(nextTasks) ? nextTasks : [];
  $('#task-count').textContent = tasks.length;
  $('#running-count').textContent = tasks.filter(item => ['queued', 'running'].includes(item.status)).length;
  renderTasks();
}

// 同一时间只允许一轮刷新，避免慢请求返回顺序相反而覆盖新数据。
async function refresh() {
  if (refreshInFlight) return refreshInFlight;
  const button = $('#refresh');
  button.disabled = true;
  setSyncStatus('busy', '同步中');
  refreshInFlight = Promise.all([loadClients(), loadTasks()]).then(() => {
    setSyncStatus('success', `已更新 ${new Date().toLocaleTimeString()}`);
  }).catch(error => {
    setSyncStatus('error', '同步失败');
    showToast(error.message, 'error', 6000);
    throw error;
  }).finally(() => {
    button.disabled = false;
    refreshInFlight = null;
  });
  return refreshInFlight;
}

// 对话框只负责采集输入；真正的状态变更全部通过带 CSRF 的 JSON API 完成。
$('#refresh').addEventListener('click', () => refresh().catch(() => {}));
$('#new-client').addEventListener('click', () => {
  $('#client-error').classList.add('hidden');
  $('#client-dialog').showModal();
  $('#client-form input[name="name"]').focus();
});
document.querySelectorAll('.close-dialog').forEach(button => button.addEventListener('click', () => $('#client-dialog').close()));
document.querySelectorAll('.close-token').forEach(button => button.addEventListener('click', () => $('#token-dialog').close()));
document.querySelectorAll('.close-edit-client').forEach(button => button.addEventListener('click', () => $('#edit-client-dialog').close()));

// Client 选择工具栏减少大量节点时的重复点击，并在每次勾选后即时显示数量。
$('#client-options').addEventListener('change', updateSelectionSummary);
$('#select-online').addEventListener('click', () => {
  document.querySelectorAll('#client-options input[name="client_id"]').forEach(input => {
    const client = clients.find(item => item.id === input.value);
    input.checked = !input.disabled && Boolean(client?.online);
  });
  updateSelectionSummary();
});
$('#select-all').addEventListener('click', () => {
  document.querySelectorAll('#client-options input[name="client_id"]:not(:disabled)').forEach(input => { input.checked = true; });
  updateSelectionSummary();
});
$('#clear-selection').addEventListener('click', () => {
  document.querySelectorAll('#client-options input[name="client_id"]').forEach(input => { input.checked = false; });
  updateSelectionSummary();
});

// 创建 Client，并把服务端仅返回一次的原始 Token 转交给专用展示对话框。
$('#client-form').addEventListener('submit', async event => {
  event.preventDefault();
  // currentTarget 在 await 之后由浏览器清空，先保存 DOM 引用以便 reset 和恢复按钮状态。
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const errorBox = $('#client-error');
  const submit = formElement.querySelector('button[type="submit"]');
  const labels = {};
  String(form.get('labels') || '').split(',').map(value => value.trim()).filter(Boolean).forEach(pair => {
    const [key, ...rest] = pair.split('=');
    if (key && rest.length) labels[key.trim()] = rest.join('=').trim();
  });
  errorBox.classList.add('hidden');
  submit.disabled = true;
  submit.textContent = '创建中…';
  try {
    const result = await api('/api/clients', {method:'POST', body: JSON.stringify({name: form.get('name'), labels})});
    $('#client-dialog').close();
    $('#client-token').value = result.token;
    $('#client-command').value = windowsClientCommand(result.token);
    $('#copy-token').textContent = '复制 Token';
    $('#copy-client-command').textContent = '复制运行命令';
    $('#token-dialog').showModal();
    formElement.reset();
    await loadClients({force: true});
    showToast('Client 已创建，请立即保存 Token。', 'success', 6000);
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally {
    submit.disabled = false;
    submit.textContent = '创建';
  }
});

$('#copy-token').addEventListener('click', async () => {
  try {
    await copyText($('#client-token').value);
    $('#copy-token').textContent = '已复制';
    showToast('Token 已复制。', 'success');
  } catch (_) { showToast('浏览器拒绝访问剪贴板，请手动复制。', 'error'); }
});

$('#copy-client-command').addEventListener('click', async () => {
  try {
    await copyText($('#client-command').value);
    $('#copy-client-command').textContent = '已复制';
    showToast('Windows 运行命令已复制。', 'success');
  } catch (_) { showToast('浏览器拒绝访问剪贴板，请手动复制。', 'error'); }
});

// 吊销会使数据库认证和当前 WebSocket 连接同时失效，历史结果仍保留 Client 关联。
$('#clients-body').addEventListener('click', async event => {
  const editButton = event.target.closest('.edit-client');
  if (editButton) {
    const client = clients.find(item => item.id === editButton.dataset.id);
    if (!client || !client.manageable) return;
    const form = $('#edit-client-form');
    form.querySelector('[name="id"]').value = client.id;
    form.querySelector('[name="name"]').value = client.name;
    form.querySelector('[name="labels"]').value = Object.entries(client.labels || {}).map(([key, value]) => `${key}=${value}`).join(', ');
    $('#edit-client-error').classList.add('hidden');
    $('#edit-client-dialog').showModal();
    form.querySelector('[name="name"]').focus();
    return;
  }
  const button = event.target.closest('.revoke-client');
  if (!button || !confirm('确认吊销此 Client Token？')) return;
  button.disabled = true;
  try { await api(`/api/clients/${button.dataset.id}`, {method:'DELETE'}); await loadClients({force: true}); showToast('Client Token 已吊销。', 'success'); }
  catch (error) { button.disabled = false; showToast(error.message, 'error'); }
});

$('#edit-client-form').addEventListener('submit', async event => {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const labels = {};
  String(form.get('labels') || '').split(',').map(value => value.trim()).filter(Boolean).forEach(pair => {
    const [key, ...rest] = pair.split('=');
    if (key && rest.length) labels[key.trim()] = rest.join('=').trim();
  });
  const errorBox = $('#edit-client-error');
  const submit = formElement.querySelector('button[type="submit"]');
  errorBox.classList.add('hidden');
  submit.disabled = true;
  try {
    await api(`/api/clients/${encodeURIComponent(form.get('id'))}`, {method:'PATCH', body:JSON.stringify({name:form.get('name'), labels})});
    $('#edit-client-dialog').close();
    await loadClients({force: true});
    showToast('测速节点信息已更新。', 'success');
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally {
    submit.disabled = false;
  }
});

// 来源输入的字数和剪贴板操作让批量导入反馈更及时；读取剪贴板失败不影响手动粘贴。
function updateSourceCount() {
  const length = $('#source-input').value.length;
  $('#source-count').textContent = `${length.toLocaleString()} 字符`;
}
$('#source-input').addEventListener('input', updateSourceCount);
$('#clear-source').addEventListener('click', () => { $('#source-input').value = ''; updateSourceCount(); $('#source-input').focus(); });
$('#paste-source').addEventListener('click', async () => {
  try {
    $('#source-input').value = await navigator.clipboard.readText();
    updateSourceCount();
    $('#source-input').focus();
    showToast('已从剪贴板粘贴代理内容。', 'success');
  } catch (_) { showToast('无法读取剪贴板，请使用 Ctrl/Cmd+V 粘贴。', 'error'); }
});

$('#task-filter').addEventListener('change', renderTasks);
$('#task-search').addEventListener('input', renderTasks);

function syncTopNOptions() {
  const candidates = Number($('#task-form select[name="candidate_count"]').value);
  const topN = $('#task-form select[name="top_n"]');
  [...topN.options].forEach(option => { option.disabled = Number(option.value) > candidates; });
  if (Number(topN.value) > candidates) topN.value = String(Math.min(3, candidates));
}
$('#task-form select[name="candidate_count"]').addEventListener('change', syncTopNOptions);

// 将任务表单组装为 API 模型；本地先校验明显缺项，减少无效请求和等待。
$('#task-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const errorBox = $('#task-error');
  const submit = event.currentTarget.querySelector('button[type="submit"]');
  const source = String(form.get('source') || '').trim();
  const subscriptionURL = String(form.get('subscription_url') || '').trim();
  const clientIDs = form.getAll('client_id');
  errorBox.classList.add('hidden');
  if (!source && !subscriptionURL) {
    errorBox.textContent = '请粘贴代理内容或填写订阅 URL。';
    errorBox.classList.remove('hidden');
    $('#source-input').focus();
    return;
  }
  if (!clientIDs.length) {
    errorBox.textContent = '请至少选择一个 Client。';
    errorBox.classList.remove('hidden');
    $('#client-options').scrollIntoView({behavior:'smooth', block:'center'});
    return;
  }
  submit.disabled = true;
  submit.textContent = '创建中…';
  $('#task-form-state').textContent = '提交中';
  $('#task-form-state').className = 'sync-status busy';
  try {
    const result = await api('/api/tasks', {method:'POST', body:JSON.stringify({
      source, subscription_url: subscriptionURL, client_ids: clientIDs,
      candidate_count: Number(form.get('candidate_count')), top_n: Number(form.get('top_n')), threads: Number(form.get('threads'))
    })});
    window.location.href = `/tasks/${result.task.id}`;
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
    $('#task-form-state').textContent = '可重试';
    $('#task-form-state').className = 'sync-status error';
  } finally {
    submit.disabled = false;
    submit.textContent = '创建任务';
  }
});

// 首屏立即加载；之后低频轮询用于修正断线或错过事件造成的状态差异。
updateSourceCount();
syncTopNOptions();
refresh().catch(() => {});
setInterval(() => refresh().catch(() => {}), 15000);
