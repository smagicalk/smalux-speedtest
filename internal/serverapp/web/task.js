const csrf = document.querySelector('meta[name="csrf-token"]').content;
const taskID = document.querySelector('meta[name="task-id"]').content;
let results = [];
let taskData = null;

const escapeHTML = value => String(value ?? '').replace(/[&<>'"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[c]));
const statusText = value => ({queued:'排队中',running:'进行中',completed:'完成',partial:'部分完成',failed:'失败',canceled:'已取消'}[value] || value);
const speed = value => value > 0 ? `${(value / 1_000_000).toFixed(2)} Mbps` : '—';
const delay = value => value > 0 ? `${value.toFixed(2)} ms` : '—';

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
    document.querySelector('#progress-phase').textContent = statusText(data.task.status);
    document.querySelector('#progress-bar').style.width = '100%';
  }
  renderResults();
}

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

document.querySelector('#export-image').addEventListener('click', () => exportResultImage());

function exportResultImage() {
  if (!taskData || results.length === 0) return;
  const sorted = [...results].sort((a, b) => `${a.proxy_name}|${a.client_name}`.localeCompare(`${b.proxy_name}|${b.client_name}`) || a.latency_ms - b.latency_ms);
  const columns = [70, 260, 180, 120, 300, 130, 130, 190, 190, 230];
  const headers = ['序号', '代理节点', 'Client', '协议', 'Speedtest 测速节点', '延迟', '抖动', '下载速度', '上传速度', '状态'];
  const width = columns.reduce((sum, value) => sum + value, 0);
  const titleHeight = 92;
  const headerHeight = 58;
  const rowHeight = 58;
  const footerHeight = 104;
  const height = titleHeight + headerHeight + rowHeight * sorted.length + footerHeight;
  const canvas = document.createElement('canvas');
  canvas.width = width;
  canvas.height = height;
  const ctx = canvas.getContext('2d');
  ctx.textBaseline = 'middle';
  ctx.fillStyle = '#f7f8f9';
  ctx.fillRect(0, 0, width, height);

  ctx.fillStyle = '#172126';
  ctx.font = '700 30px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
  ctx.textAlign = 'center';
  ctx.fillText('Smalux Speedtest · 分布式代理测速', width / 2, 34);
  ctx.fillStyle = '#66737a';
  ctx.font = '17px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
  ctx.fillText(`任务 ${taskID.slice(0, 12)} · ${taskData.client_count} 个 Client · ${taskData.proxy_count} 个代理`, width / 2, 68);

  let x = 0;
  ctx.fillStyle = '#e8ecee';
  ctx.fillRect(0, titleHeight, width, headerHeight);
  ctx.font = '700 17px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
  ctx.fillStyle = '#29343a';
  headers.forEach((header, index) => {
    ctx.fillText(header, x + columns[index] / 2, titleHeight + headerHeight / 2);
    x += columns[index];
  });

  const maxLatency = Math.max(...sorted.map(item => item.latency_ms || 0), 1);
  const maxJitter = Math.max(...sorted.map(item => item.jitter_ms || 0), 1);
  const maxDownload = Math.max(...sorted.map(item => item.download_bps || 0), 1);
  const maxUpload = Math.max(...sorted.map(item => item.upload_bps || 0), 1);
  const groups = [];
  sorted.forEach((item, index) => {
    const key = `${item.proxy_id}|${item.client_id}`;
    const previous = groups[groups.length - 1];
    if (previous?.key === key) previous.count += 1;
    else groups.push({key, start:index, count:1, item});
  });

  sorted.forEach((item, row) => {
    const y = titleHeight + headerHeight + row * rowHeight;
    ctx.fillStyle = row % 2 ? '#fbfcfc' : '#ffffff';
    ctx.fillRect(0, y, width, rowHeight);
    const offsets = columnOffsets(columns);
    heatCell(ctx, offsets[5], y, columns[5], rowHeight, item.latency_ms / maxLatency, '#72d2dc');
    heatCell(ctx, offsets[6], y, columns[6], rowHeight, item.jitter_ms / maxJitter, '#72d2dc');
    heatCell(ctx, offsets[7], y, columns[7], rowHeight, item.download_bps / maxDownload, '#ff3f7d', true);
    heatCell(ctx, offsets[8], y, columns[8], rowHeight, item.upload_bps / maxUpload, '#ff3f7d', true);

    const cells = [
      '', '', '', '', `${item.speed_server_name || '—'}${item.sponsor ? ` · ${item.sponsor}` : ''}`,
      formatDelay(item.latency_ms), formatDelay(item.jitter_ms), formatImageSpeed(item.download_bps), formatImageSpeed(item.upload_bps), item.error || '完成'
    ];
    ctx.font = '16px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
    ctx.fillStyle = item.error ? '#a82424' : '#172126';
    cells.forEach((value, index) => drawCanvasText(ctx, value, offsets[index], y, columns[index], rowHeight));
    ctx.strokeStyle = '#dce2e5';
    ctx.lineWidth = 1;
    ctx.beginPath();
    ctx.moveTo(offsets[4], y + rowHeight - .5);
    ctx.lineTo(width, y + rowHeight - .5);
    ctx.stroke();
  });

  const offsets = columnOffsets(columns);
  groups.forEach((group, index) => {
    const y = titleHeight + headerHeight + group.start * rowHeight;
    const groupHeight = group.count * rowHeight;
    const values = [String(index + 1), group.item.proxy_name, group.item.client_name || group.item.client_id.slice(0, 8), group.item.protocol.toUpperCase()];
    ctx.fillStyle = '#172126';
    ctx.font = '17px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
    values.forEach((value, cell) => drawCanvasText(ctx, value, offsets[cell], y, columns[cell], groupHeight, cell === 1));
    ctx.strokeStyle = '#cfd7da';
    ctx.beginPath();
    ctx.moveTo(0, y + groupHeight - .5);
    ctx.lineTo(offsets[4], y + groupHeight - .5);
    ctx.stroke();
  });

  x = 0;
  ctx.strokeStyle = '#d4dade';
  ctx.beginPath();
  columns.forEach(column => { x += column; ctx.moveTo(x - .5, titleHeight); ctx.lineTo(x - .5, height - footerHeight); });
  ctx.stroke();

  ctx.save();
  ctx.translate(width / 2, titleHeight + headerHeight + (sorted.length * rowHeight) / 2);
  ctx.rotate(-0.28);
  ctx.globalAlpha = 0.055;
  ctx.fillStyle = '#087f5b';
  ctx.font = '800 68px Arial, sans-serif';
  ctx.fillText('SMALUX SPEEDTEST', 0, 0);
  ctx.restore();

  const footerY = height - footerHeight;
  ctx.fillStyle = '#e8ecee';
  ctx.fillRect(0, footerY, width, footerHeight);
  ctx.textAlign = 'left';
  ctx.fillStyle = '#29343a';
  ctx.font = '17px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
  ctx.fillText(`线程=${taskData.threads}   候选节点=${taskData.candidate_count}   上下行节点=Top ${taskData.top_n}   结果=${sorted.length} 条`, 22, footerY + 32);
  ctx.fillStyle = '#66737a';
  ctx.font = '15px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
  ctx.fillText(`测试时间：${new Date(taskData.created_at).toLocaleString()}   测试结果仅供参考，以实际网络情况为准`, 22, footerY + 72);

  canvas.toBlob(blob => {
    if (!blob) return;
    const link = document.createElement('a');
    link.href = URL.createObjectURL(blob);
    link.download = `smalux-speedtest-${taskID.slice(0, 8)}.png`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(link.href), 1000);
  }, 'image/png');
}

function columnOffsets(columns) {
  const offsets = [];
  let current = 0;
  columns.forEach(width => { offsets.push(current); current += width; });
  return offsets;
}

function heatCell(ctx, x, y, width, height, ratio, color, bar = false) {
  const normalized = Math.max(0, Math.min(1, ratio || 0));
  ctx.save();
  ctx.globalAlpha = .15 + normalized * .58;
  ctx.fillStyle = color;
  ctx.fillRect(x, y, width, height);
  if (bar && normalized > 0) {
    ctx.globalAlpha = .9;
    ctx.fillRect(x + 8, y + height - 8, (width - 16) * normalized, 4);
  }
  ctx.restore();
}

function drawCanvasText(ctx, value, x, y, width, height, bold = false) {
  ctx.save();
  ctx.beginPath();
  ctx.rect(x + 6, y, width - 12, height);
  ctx.clip();
  if (bold) ctx.font = '700 18px Arial, "Microsoft YaHei", "Noto Sans CJK SC", sans-serif';
  ctx.textAlign = 'center';
  const text = trimCanvasText(ctx, String(value), width - 18);
  ctx.fillText(text, x + width / 2, y + height / 2);
  ctx.restore();
}

function trimCanvasText(ctx, value, maxWidth) {
  if (ctx.measureText(value).width <= maxWidth) return value;
  let output = value;
  while (output.length && ctx.measureText(`${output}…`).width > maxWidth) output = output.slice(0, -1);
  return `${output}…`;
}

function formatDelay(value) { return value > 0 ? `${value.toFixed(1)} ms` : '—'; }
function formatImageSpeed(value) {
  if (!value || value <= 0) return '—';
  return value >= 1_000_000_000 ? `${(value / 1_000_000_000).toFixed(2)} Gbps` : `${(value / 1_000_000).toFixed(2)} Mbps`;
}

document.querySelector('#cancel-task').addEventListener('click', async () => {
  if (!confirm('确认取消此任务？')) return;
  const response = await fetch(`/api/tasks/${taskID}/cancel`, {method:'POST', headers:{'X-CSRF-Token':csrf}});
  if (!response.ok) alert((await response.json()).error);
  await loadTask();
});

const events = new EventSource(`/api/tasks/${taskID}/events`);
events.onmessage = event => {
  const update = JSON.parse(event.data);
  if (update.type === 'progress') {
    const progress = update.progress;
    document.querySelector('#progress-phase').textContent = progress.phase || '测速中';
    document.querySelector('#progress-message').textContent = [progress.proxy_name, progress.message].filter(Boolean).join(' · ');
    if (progress.total > 0) document.querySelector('#progress-bar').style.width = `${Math.min(100, progress.current / progress.total * 100)}%`;
  } else if (update.type === 'result') {
    const index = results.findIndex(item => item.client_id === update.result.client_id && item.proxy_id === update.result.proxy_id && item.speed_server_id === update.result.speed_server_id);
    if (index >= 0) results[index] = update.result; else results.push(update.result);
    renderResults();
  } else if (update.type === 'status') {
    loadTask().catch(() => {});
    if (!['queued','running'].includes(update.status)) events.close();
  }
};

loadTask().catch(error => alert(error.message));
