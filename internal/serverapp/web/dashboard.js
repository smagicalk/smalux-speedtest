const csrf = document.querySelector('meta[name="csrf-token"]').content;
let clients = [];

const api = async (url, options = {}) => {
  const headers = { ...(options.headers || {}) };
  if (options.body) headers['Content-Type'] = 'application/json';
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch(url, { ...options, headers });
  const data = response.status === 204 ? null : await response.json();
  if (!response.ok) throw new Error(data?.error || `HTTP ${response.status}`);
  return data;
};

const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]));
const dateText = value => value ? new Date(value).toLocaleString() : '—';
const statusText = value => ({queued:'排队中',running:'进行中',completed:'完成',partial:'部分完成',failed:'失败',canceled:'已取消'}[value] || value);

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

async function refresh() {
  const button = document.querySelector('#refresh');
  button.disabled = true;
  try { await Promise.all([loadClients(), loadTasks()]); } finally { button.disabled = false; }
}

document.querySelector('#refresh').addEventListener('click', refresh);
document.querySelector('#new-client').addEventListener('click', () => document.querySelector('#client-dialog').showModal());
document.querySelectorAll('.close-dialog').forEach(button => button.addEventListener('click', () => document.querySelector('#client-dialog').close()));
document.querySelectorAll('.close-token').forEach(button => button.addEventListener('click', () => document.querySelector('#token-dialog').close()));

document.querySelector('#client-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const labels = {};
  String(form.get('labels') || '').split(',').map(value => value.trim()).filter(Boolean).forEach(pair => {
    const [key, ...rest] = pair.split('=');
    if (key && rest.length) labels[key.trim()] = rest.join('=').trim();
  });
  try {
    const result = await api('/api/clients', {method:'POST', body: JSON.stringify({name: form.get('name'), labels})});
    document.querySelector('#client-dialog').close();
    document.querySelector('#client-token').value = result.token;
    document.querySelector('#token-dialog').showModal();
    event.currentTarget.reset();
    await loadClients();
  } catch (error) { alert(error.message); }
});

document.querySelector('#copy-token').addEventListener('click', async () => {
  await navigator.clipboard.writeText(document.querySelector('#client-token').value);
  document.querySelector('#copy-token').textContent = '已复制';
});

document.querySelector('#clients-body').addEventListener('click', async event => {
  const button = event.target.closest('.revoke-client');
  if (!button || !confirm('确认吊销此 Client Token？')) return;
  try { await api(`/api/clients/${button.dataset.id}`, {method:'DELETE'}); await loadClients(); } catch (error) { alert(error.message); }
});

document.querySelector('#task-form').addEventListener('submit', async event => {
  event.preventDefault();
  const form = new FormData(event.currentTarget);
  const errorBox = document.querySelector('#task-error');
  const submit = event.currentTarget.querySelector('button[type="submit"]');
  errorBox.classList.add('hidden');
  submit.disabled = true;
  try {
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

refresh().catch(error => alert(error.message));
setInterval(() => Promise.all([loadClients(), loadTasks()]).catch(() => {}), 15000);
