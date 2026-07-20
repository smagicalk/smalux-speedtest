import { exportResultImage } from './report.js';
import { $, copyText, escapeHTML, redirectToLogin, showToast, statusText } from './ui.js';

// 服务端模板注入当前管理员会话的 CSRF 令牌和不可变任务 ID。
const csrf = document.querySelector('meta[name="csrf-token"]').content;
const taskID = document.querySelector('meta[name="task-id"]').content;
// results 会被初始快照和 SSE 增量消息共同更新；taskData 同时作为图片报告元数据来源。
let results = [];
let taskData = null;
let loadInFlight = null;
let events = null;
// SSE 可能在 REST 快照返回前到达；revision 用来识别这类“快照落后于实时事件”的响应。
let liveRevision = 0;
let liveStatus = null;
const terminalStatuses = new Set(['completed', 'partial', 'failed', 'canceled']);

function setSyncStatus(state, text) {
  const status = $('#task-sync-status');
  if (!status) return;
  status.className = `sync-status ${state}`;
  status.textContent = text;
}

function isTerminalStatus(value) {
  return terminalStatuses.has(value);
}

function statusRank(value) {
  if (value === 'queued') return 0;
  if (value === 'running') return 1;
  if (isTerminalStatus(value)) return 2;
  return -1;
}

function speed(value) {
  const number = Number(value);
  return number > 0 ? `${(number / 1_000_000).toFixed(2)} Mbps` : '—';
}

function delay(value) {
  const number = Number(value);
  return number > 0 ? `${number.toFixed(2)} ms` : '—';
}

function expectedResults() {
  if (!taskData) return 0;
  const expected = Number(taskData.proxy_count) * Number(taskData.client_count) * Number(taskData.top_n);
  return Number.isFinite(expected) ? Math.max(0, expected) : 0;
}

function updateProgress(progress = null) {
  if (!taskData) return;
  const terminal = isTerminalStatus(taskData.status);
  const expected = expectedResults();
  const reportedTotal = Number(progress?.total);
  const reportedCurrent = Number(progress?.current);
  const hasProgress = progress && Number.isFinite(reportedTotal) && reportedTotal > 0 && Number.isFinite(reportedCurrent);
  const total = hasProgress ? reportedTotal : expected;
  const current = hasProgress ? Math.max(0, Math.min(reportedCurrent, total)) : results.length;
  const percentage = terminal ? 100 : (total > 0 ? Math.min(100, Math.max(0, current / total * 100)) : 0);
  $('#progress-bar').style.width = `${percentage}%`;
  $('#progress-count').textContent = hasProgress ? `阶段 ${current} / ${total} · ${results.length} 条结果` : `${results.length} 条结果${expected ? ` · 预计 ${expected}` : ''}`;
  $('#progress-updated').textContent = `更新于 ${new Date().toLocaleTimeString()}`;
  if (progress) {
    $('#progress-phase').textContent = progress.phase || '测速中';
    $('#progress-message').textContent = [progress.proxy_name, progress.message].filter(Boolean).join(' · ') || '正在执行';
  } else if (terminal) {
    $('#progress-phase').textContent = statusText(taskData.status);
    $('#progress-message').textContent = taskData.error || `已收到 ${results.length} 条结果`;
  } else {
    $('#progress-phase').textContent = statusText(taskData.status);
    $('#progress-message').textContent = taskData.status === 'queued' ? '任务已进入队列，等待 Client' : 'Client 正在执行测速';
  }
}

// 获取任务和结果的权威快照；并发刷新合并为一次，避免慢响应覆盖新状态。
async function loadTask({silent = false} = {}) {
  if (loadInFlight) return loadInFlight;
  if (!silent) setSyncStatus('busy', '刷新任务');
  const requestRevision = liveRevision;
  loadInFlight = (async () => {
    const response = await fetch(`/api/tasks/${taskID}`, {cache: 'no-store'});
    if (redirectToLogin(response)) throw new Error('登录已失效');
    if (!response.ok) throw new Error(response.status === 404 ? '任务不存在' : `任务读取失败（${response.status}）`);
    const data = await response.json();
    let nextTask = data.task;
    let nextResults = data.results || [];
    // 请求期间若收到 SSE，REST 响应可能是旧快照。保留实时事件带来的结果，并让
    // 已知终态不会被旧的 running/queued 响应回滚；没有并发事件时直接采用权威快照。
    const currentStatus = liveStatus || taskData?.status;
    if (liveRevision !== requestRevision) nextResults = mergeResults(nextResults, results);
    // 状态只允许向前推进，即使本次 REST 请求本身没有观察到新的 revision；这覆盖
    // 终态事件后紧接着返回旧 running 快照的窗口。
    if (currentStatus && statusRank(currentStatus) >= statusRank(nextTask.status)) {
      nextTask = {...nextTask, status: currentStatus};
      if (taskData?.error && !nextTask.error) nextTask.error = taskData.error;
    }
    taskData = nextTask;
    results = nextResults;
    $('#task-meta').innerHTML = `<span class="status ${escapeHTML(taskData.status)}">${escapeHTML(statusText(taskData.status))}</span> · ${taskData.proxy_count} 个代理 · ${taskData.client_count} 个 Client · ${taskData.threads} 线程 · Top ${taskData.top_n}/${taskData.candidate_count}`;
    $('#cancel-task').disabled = !['queued','running'].includes(taskData.status);
    $('#cancel-task').textContent = '取消任务';
    renderResults();
    updateProgress();
    if (isTerminalStatus(taskData.status)) events?.close();
    setSyncStatus('success', `已更新 ${new Date().toLocaleTimeString()}`);
    return data;
  })().catch(error => {
    setSyncStatus('error', '同步失败');
    if (!silent) showToast(error.message, 'error', 6000);
    throw error;
  }).finally(() => { loadInFlight = null; });
  return loadInFlight;
}

// 用数据库唯一键合并 REST 快照与 SSE 增量，避免旧 HTTP 响应抹掉刚显示的结果。
function mergeResults(snapshot, live) {
  const merged = [...snapshot];
  const indexes = new Map(merged.map((item, index) => [resultKey(item), index]));
  live.forEach(item => {
    const key = resultKey(item);
    const index = indexes.get(key);
    if (index !== undefined) {
      merged[index] = item;
    } else {
      indexes.set(key, merged.length);
      merged.push(item);
    }
  });
  return merged;
}

function resultKey(item) {
  return `${item.client_id}\u0000${item.proxy_id}\u0000${item.speed_server_id}`;
}

// 根据当前内存快照重建结果表。SSE 更新单条结果后也复用此函数，
// 因而展示逻辑不会因“初始加载”或“实时到达”产生差异。
function renderResults() {
  $('#result-count').textContent = `${results.length} 条结果`;
  $('#results-body').innerHTML = results.length ? results.map(item => `
    <tr>
      <td><strong>${escapeHTML(item.proxy_name)}</strong><br><span class="mono muted">${escapeHTML(item.masked_address)}</span></td>
      <td>${escapeHTML(item.client_name || String(item.client_id || '').slice(0, 8))}</td>
      <td>${escapeHTML(item.protocol)}</td>
      <td>${escapeHTML(item.speed_server_name || '—')}<br><span class="muted">${escapeHTML([item.sponsor,item.country].filter(Boolean).join(' · '))}</span></td>
      <td>${delay(item.latency_ms)}</td><td>${delay(item.jitter_ms)}</td>
      <td class="rate">${speed(item.download_bps)}</td><td class="rate">${speed(item.upload_bps)}</td>
      <td>${item.error ? `<span class="error-text" title="${escapeHTML(item.error)}">${escapeHTML(item.error)}</span>` : '<span class="status completed">完成</span>'}</td>
    </tr>`).join('') : '<tr><td colspan="9" class="empty">等待测速结果</td></tr>';
  $('#export-image').disabled = !taskData || (!isTerminalStatus(taskData.status) && results.length === 0);
  const expected = expectedResults();
  $('#progress-count').textContent = `${results.length} 条结果${expected ? ` · 预计 ${expected}` : ''}`;
}

$('#refresh-task').addEventListener('click', () => loadTask().catch(() => {}));
$('#copy-task-id').addEventListener('click', async () => {
  try {
    await copyText(taskID);
    $('#copy-task-id').textContent = '已复制';
    showToast('任务 ID 已复制。', 'success');
    window.setTimeout(() => { $('#copy-task-id').textContent = '复制 ID'; }, 1800);
  } catch (_) { showToast('浏览器拒绝访问剪贴板，请手动复制。', 'error'); }
});

$('#export-image').addEventListener('click', () => {
  if (taskData && (results.length || isTerminalStatus(taskData.status))) exportResultImage(taskData, results, taskID);
});

// 取消操作需要显式确认；请求期间锁定按钮，避免重复提交和重复通知 Client。
$('#cancel-task').addEventListener('click', async () => {
  if (!confirm('确认取消此任务？')) return;
  const button = $('#cancel-task');
  button.disabled = true;
  button.textContent = '取消中…';
  try {
    const response = await fetch(`/api/tasks/${taskID}/cancel`, {method:'POST', headers:{'X-CSRF-Token':csrf}});
    if (redirectToLogin(response)) throw new Error('登录已失效');
    let payload = null;
    try { payload = await response.json(); } catch (_) { /* 由通用错误消息兜底。 */ }
    if (!response.ok) throw new Error(payload?.error || `取消失败（${response.status}）`);
    showToast('已请求取消任务。', 'success');
    await loadTask();
  } catch (error) {
    button.disabled = false;
    button.textContent = '取消任务';
    showToast(error.message, 'error');
  }
});

// SSE 负责实时更新；断线时保留页面数据并显示“重连中”，EventSource 会自动重试。
function connectEvents() {
  events = new EventSource(`/api/tasks/${taskID}/events`);
  events.onopen = () => setSyncStatus('success', '实时连接已建立');
  events.onerror = () => {
    if (taskData && !['queued', 'running'].includes(taskData.status)) return;
    setSyncStatus('busy', '实时连接重试中');
  };
  events.onmessage = event => {
    let update;
    try { update = JSON.parse(event.data); } catch (_) { return; }
    if (update.type === 'progress') {
      liveRevision++;
      updateProgress(update.progress);
    } else if (update.type === 'result' && update.result) {
      liveRevision++;
      // 结果唯一键与数据库一致：Client + 代理 + Speedtest 节点。
      const item = update.result;
      const key = resultKey(item);
      const index = results.findIndex(result => resultKey(result) === key);
      if (index >= 0) results[index] = item; else results.push(item);
      renderResults();
      updateProgress();
    } else if (update.type === 'status') {
      liveRevision++;
      liveStatus = update.status;
      // 先把状态写入内存，再让 REST 快照合并；这样旧响应不会把终态回滚为 running。
      if (taskData) {
        taskData = {...taskData, status: update.status};
        updateProgress();
      }
      // 终态时重新取完整快照，确认合并完成后再关闭流，避免旧请求落后于事件。
      loadTask().then(() => {
        if (taskData && isTerminalStatus(taskData.status)) events?.close();
      }).catch(() => {});
    }
  };
}

connectEvents();
loadTask().catch(() => {});
// SSE 只推送变化，低频复核可覆盖代理重启或事件拥塞造成的短暂差异。
const refreshTimer = window.setInterval(() => loadTask({silent: true}).catch(() => {}), 12000);
window.addEventListener('beforeunload', () => {
  window.clearInterval(refreshTimer);
  events?.close();
});
