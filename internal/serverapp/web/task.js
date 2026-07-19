import { exportResultImage } from './report.js';

// 服务端模板注入当前管理员会话的 CSRF 令牌和不可变任务 ID。
const csrf = document.querySelector('meta[name="csrf-token"]').content;
const taskID = document.querySelector('meta[name="task-id"]').content;
// results 会被初始快照和 SSE 增量消息共同更新；taskData 同时作为图片报告元数据来源。
let results = [];
let taskData = null;

// 结果表使用 innerHTML 渲染，所有服务端字符串必须经过转义以隔离代理名称等不可信输入。
const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]));
const statusText = value => ({queued:'排队中',running:'进行中',completed:'完成',partial:'部分完成',failed:'失败',canceled:'已取消'}[value] || value);
const speed = value => value > 0 ? `${(value / 1_000_000).toFixed(2)} Mbps` : '—';
const delay = value => value > 0 ? `${value.toFixed(2)} ms` : '—';

// 获取任务和结果的权威快照，供首屏加载、终态收敛及 SSE 丢事件后的修正使用。
async function loadTask() {
  const response = await fetch(`/api/tasks/${taskID}`);
  if (!response.ok) throw new Error('任务不存在');
  const data = await response.json();
  taskData = data.task;
  results = data.results || [];
  document.querySelector('#task-meta').innerHTML = `<span class="status ${escapeHTML(data.task.status)}">${escapeHTML(statusText(data.task.status))}</span> · ${data.task.proxy_count} 个代理 · ${data.task.client_count} 个 Client · ${data.task.threads} 线程 · Top ${data.task.top_n}/${data.task.candidate_count}`;
  document.querySelector('#cancel-task').disabled = !['queued','running'].includes(data.task.status);
  document.querySelector('#export-image').disabled = results.length === 0;
  if (!['queued','running'].includes(data.task.status)) {
    // 终态任务不再接收有效进度，将进度条收束到 100% 作为明确的视觉结束状态。
    document.querySelector('#progress-phase').textContent = statusText(data.task.status);
    document.querySelector('#progress-bar').style.width = '100%';
  }
  renderResults();
}

// 根据当前内存快照重建结果表。SSE 更新单条结果后也复用此函数，
// 因而展示逻辑不会因“初始加载”或“实时到达”产生差异。
function renderResults() {
  document.querySelector('#result-count').textContent = `${results.length} 条结果`;
  document.querySelector('#results-body').innerHTML = results.length ? results.map(item => `
    <tr>
      <td><strong>${escapeHTML(item.proxy_name)}</strong><br><span class="mono muted">${escapeHTML(item.masked_address)}</span></td>
      <td>${escapeHTML(item.client_name || item.client_id.slice(0, 8))}</td>
      <td>${escapeHTML(item.protocol)}</td>
      <td>${escapeHTML(item.speed_server_name || '—')}<br><span class="muted">${escapeHTML([item.sponsor,item.country].filter(Boolean).join(' · '))}</span></td>
      <td>${delay(item.latency_ms)}</td><td>${delay(item.jitter_ms)}</td>
      <td class="rate">${speed(item.download_bps)}</td><td class="rate">${speed(item.upload_bps)}</td>
      <td>${item.error ? `<span class="error-text" title="${escapeHTML(item.error)}">${escapeHTML(item.error)}</span>` : '<span class="status completed">完成</span>'}</td>
    </tr>`).join('') : '<tr><td colspan="9" class="empty">等待测速结果</td></tr>';
  document.querySelector('#export-image').disabled = results.length === 0;
}

document.querySelector('#export-image').addEventListener('click', () => exportResultImage(taskData, results, taskID));

// 取消操作需要显式确认并携带 CSRF；随后立即刷新快照显示服务端聚合后的状态。
document.querySelector('#cancel-task').addEventListener('click', async () => {
  if (!confirm('确认取消此任务？')) return;
  const response = await fetch(`/api/tasks/${taskID}/cancel`, {method:'POST', headers:{'X-CSRF-Token':csrf}});
  if (!response.ok) alert((await response.json()).error);
  await loadTask();
});

// SSE 负责服务端到浏览器的单向实时更新；断线重连由 EventSource 原生实现。
// 页面初始快照仍由 loadTask 获取，避免连接建立前遗漏事件导致空白。
const events = new EventSource(`/api/tasks/${taskID}/events`);
events.onmessage = event => {
  const update = JSON.parse(event.data);
  if (update.type === 'progress') {
    const progress = update.progress;
    document.querySelector('#progress-phase').textContent = progress.phase || '测速中';
    document.querySelector('#progress-message').textContent = [progress.proxy_name, progress.message].filter(Boolean).join(' · ');
    if (progress.total > 0) document.querySelector('#progress-bar').style.width = `${Math.min(100, progress.current / progress.total * 100)}%`;
  } else if (update.type === 'result') {
    // 结果唯一键与数据库一致：Client + 代理 + Speedtest 节点。
    // 重复事件覆盖原对象，使 SSE 重连或服务端重发保持幂等。
    const index = results.findIndex(item => item.client_id === update.result.client_id && item.proxy_id === update.result.proxy_id && item.speed_server_id === update.result.speed_server_id);
    if (index >= 0) results[index] = update.result; else results.push(update.result);
    renderResults();
  } else if (update.type === 'status') {
    // 终态时重新取完整快照，再主动关闭流，避免浏览器继续执行无意义重连。
    loadTask().catch(() => {});
    if (!['queued','running'].includes(update.status)) events.close();
  }
};

loadTask().catch(error => alert(error.message));
