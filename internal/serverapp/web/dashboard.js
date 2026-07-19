// CSRF 令牌由服务端模板注入。所有改变服务端状态的请求都必须携带该值，
// 防止其他站点借用管理员浏览器中的会话发起跨站请求。
const csrf = document.querySelector('meta[name="csrf-token"]').content;
// 保存最近一次 Client 列表，供概览、表格和任务目标选择器共同使用。
let clients = [];

// api 统一处理管理端 JSON API 的请求头、错误格式和空响应。
// 只有存在 body 时才声明 JSON，DELETE 等无请求体操作不会发送多余的 Content-Type。
const api = async (url, options = {}) => {
  const headers = { ...(options.headers || {}) };
  if (options.body) headers['Content-Type'] = 'application/json';
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch(url, { ...options, headers });
  const data = response.status === 204 ? null : await response.json();
  if (!response.ok) throw new Error(data?.error || `HTTP ${response.status}`);
  return data;
};

// 表格内容通过 innerHTML 批量渲染，因此任何来自 API 或用户输入的字符串都必须先转义。
// 数值与固定状态文案不经过此函数也不会产生可执行标记。
const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]));
const dateText = value => value ? new Date(value).toLocaleString() : '—';
const statusText = value => ({queued:'排队中',running:'进行中',completed:'完成',partial:'部分完成',failed:'失败',canceled:'已取消'}[value] || value);

// 拉取 Client 的注册信息与实时在线状态，并同步刷新三个依赖同一数据源的界面区域。
async function loadClients() {
  clients = await api('/api/clients');
  document.querySelector('#online-count').textContent = clients.filter(item => item.online).length;
  document.querySelector('#client-count').textContent = clients.length;
  document.querySelector('#clients-body').innerHTML = clients.length ? clients.map(item => `
    <tr>
      <td><span class="status ${item.online ? 'online' : ''}">${item.online ? '在线' : '离线'}</span></td>
      <td><strong>${escapeHTML(item.name)}</strong></td>
      <td>${escapeHTML([item.os, item.arch].filter(Boolean).join(' / ') || '—')}</td>
      <td class="mono">${escapeHTML(item.version || '—')}</td>
      <td>${Object.entries(item.labels || {}).map(([key,value]) => `<span class="tag">${escapeHTML(key)}=${escapeHTML(value)}</span>`).join('') || '—'}</td>
      <td>${dateText(item.last_seen)}</td>
      <td><button class="danger revoke-client" data-id="${escapeHTML(item.id)}" type="button">吊销</button></td>
    </tr>`).join('') : '<tr><td colspan="7" class="empty">暂无 Client</td></tr>';
  document.querySelector('#client-options').innerHTML = clients.filter(item => item.enabled).length ? clients.filter(item => item.enabled).map(item => `
    <label class="check-option"><input type="checkbox" name="client_id" value="${escapeHTML(item.id)}"><span>${escapeHTML(item.name)}</span></label>`).join('') : '<span class="muted">暂无可用 Client</span>';
}

// 任务列表只加载服务端保留的最近 100 条记录；进行中数量同时包含 queued 与 running。
async function loadTasks() {
  const tasks = await api('/api/tasks');
  document.querySelector('#task-count').textContent = tasks.length;
  document.querySelector('#running-count').textContent = tasks.filter(item => item.status === 'queued' || item.status === 'running').length;
  document.querySelector('#tasks-body').innerHTML = tasks.length ? tasks.map(item => `
    <tr>
      <td>${dateText(item.created_at)}</td>
      <td><span class="status ${escapeHTML(item.status)}">${escapeHTML(statusText(item.status))}</span></td>
      <td>${item.proxy_count}</td><td>${item.client_count}</td><td>Top ${item.top_n} / ${item.candidate_count}</td><td>${item.threads}</td>
      <td><a class="quiet button-link" href="/tasks/${escapeHTML(item.id)}">查看</a></td>
    </tr>`).join('') : '<tr><td colspan="7" class="empty">暂无任务</td></tr>';
}

// 手动刷新期间禁用按钮，防止并发点击产生重复请求和界面回跳。
async function refresh() {
  const button = document.querySelector('#refresh');
  button.disabled = true;
  try { await Promise.all([loadClients(), loadTasks()]); } finally { button.disabled = false; }
}

// 对话框只负责采集输入；真正的状态变更全部通过带 CSRF 的 JSON API 完成。
document.querySelector('#refresh').addEventListener('click', refresh);
document.querySelector('#new-client').addEventListener('click', () => document.querySelector('#client-dialog').showModal());
document.querySelectorAll('.close-dialog').forEach(button => button.addEventListener('click', () => document.querySelector('#client-dialog').close()));
document.querySelectorAll('.close-token').forEach(button => button.addEventListener('click', () => document.querySelector('#token-dialog').close()));

// 创建 Client，并把服务端仅返回一次的原始 Token 转交给专用展示对话框。
document.querySelector('#client-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const labels = {};
  // 标签格式为逗号分隔的 key=value。value 中继续出现的等号会被保留，
  // 例如 endpoint=https://example.com?a=b 不会被截断。
  String(form.get('labels') || '').split(',').map(value => value.trim()).filter(Boolean).forEach(pair => {
    const [key, ...rest] = pair.split('=');
    if (key && rest.length) labels[key.trim()] = rest.join('=').trim();
  });
  try {
    const result = await api('/api/clients', {method:'POST', body: JSON.stringify({name: form.get('name'), labels})});
    document.querySelector('#client-dialog').close();
    // 原始 Token 只在创建响应中返回一次；数据库只保存哈希，所以必须立即展示给管理员。
    document.querySelector('#client-token').value = result.token;
    document.querySelector('#token-dialog').showModal();
    event.currentTarget.reset();
    await loadClients();
  } catch (error) { alert(error.message); }
});

// Clipboard API 只复制当前一次性 Token，不把它写入其他页面状态或持久化存储。
document.querySelector('#copy-token').addEventListener('click', async () => {
  await navigator.clipboard.writeText(document.querySelector('#client-token').value);
  document.querySelector('#copy-token').textContent = '已复制';
});

// 吊销会使数据库认证和当前 WebSocket 连接同时失效，历史结果仍保留 Client 关联。
document.querySelector('#clients-body').addEventListener('click', async event => {
  // 使用事件委托，使重新渲染出来的吊销按钮无需逐个重新绑定监听器。
  const button = event.target.closest('.revoke-client');
  if (!button || !confirm('确认吊销此 Client Token？')) return;
  try { await api(`/api/clients/${button.dataset.id}`, {method:'DELETE'}); await loadClients(); } catch (error) { alert(error.message); }
});

// 将任务表单组装为 API 模型；成功后直接进入详情页观察实时进度。
document.querySelector('#task-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const errorBox = document.querySelector('#task-error');
  const submit = event.currentTarget.querySelector('button[type="submit"]');
  errorBox.classList.add('hidden');
  submit.disabled = true;
  try {
    // source 可以是单条、批量或 Base64 订阅正文；subscription_url 由服务端按 SSRF 规则抓取。
    // Client ID 使用 getAll 保留多选结果，数值字段显式转换，避免 JSON 中出现数字字符串。
    const result = await api('/api/tasks', {method:'POST', body:JSON.stringify({
      source: form.get('source'), subscription_url: form.get('subscription_url'), client_ids: form.getAll('client_id'),
      candidate_count: Number(form.get('candidate_count')), top_n: Number(form.get('top_n')), threads: Number(form.get('threads'))
    })});
    window.location.href = `/tasks/${result.task.id}`;
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally { submit.disabled = false; }
});

// 首屏立即加载；之后低频轮询用于修正断线或错过事件造成的状态差异。
refresh().catch(error => alert(error.message));
setInterval(() => Promise.all([loadClients(), loadTasks()]).catch(() => {}), 15000);
