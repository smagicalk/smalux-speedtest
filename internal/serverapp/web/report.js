// exportResultImage 在浏览器内生成完整 PNG，不把结果再次上传给服务端。
// 报告使用固定 1800px 表格宽度保证列对齐，并按实际结果数动态扩展高度。
export function exportResultImage(taskData, results, taskID) {
  if (!taskData || results.length === 0) return;
  // 先按代理和 Client 分组，再按延迟排列同组测速节点，便于生成类似合并单元格的报告。
  const sorted = [...results].sort((a, b) => `${a.proxy_name}|${a.client_name}`.localeCompare(`${b.proxy_name}|${b.client_name}`) || a.latency_ms - b.latency_ms);
  // 列宽之和即画布宽度。固定尺寸避免内容和字体加载造成列宽抖动。
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

  // 标题区只包含项目与任务身份，具体运行参数集中放在页脚中。
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

  // 热力色按本次报告内的最大值归一化。下限 1 防止全零结果出现除零和 NaN。
  const maxLatency = Math.max(...sorted.map(item => item.latency_ms || 0), 1);
  const maxJitter = Math.max(...sorted.map(item => item.jitter_ms || 0), 1);
  const maxDownload = Math.max(...sorted.map(item => item.download_bps || 0), 1);
  const maxUpload = Math.max(...sorted.map(item => item.upload_bps || 0), 1);
  const groups = [];
  // 相邻的同代理、同 Client 结果组成一个视觉分组，左侧四列随后只绘制一次。
  sorted.forEach((item, index) => {
    const key = `${item.proxy_id}|${item.client_id}`;
    const previous = groups[groups.length - 1];
    if (previous?.key === key) previous.count += 1;
    else groups.push({key, start:index, count:1, item});
  });

  // 第一遍绘制每条测速节点记录、热力背景和右侧分隔线。
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
      // 前四列由下一阶段按组绘制，这里留空以模拟纵向合并单元格。
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
  // 第二遍在每个分组的纵向中心写入序号、代理、Client 和协议。
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

  // 统一绘制纵向网格，避免逐行描边产生重叠后颜色加深。
  x = 0;
  ctx.strokeStyle = '#d4dade';
  ctx.beginPath();
  columns.forEach(column => { x += column; ctx.moveTo(x - .5, titleHeight); ctx.lineTo(x - .5, height - footerHeight); });
  ctx.stroke();

  // 水印必须在单元格背景之后绘制，否则会被不透明行背景完全覆盖。
  ctx.save();
  ctx.translate(width / 2, titleHeight + headerHeight + (sorted.length * rowHeight) / 2);
  ctx.rotate(-0.28);
  ctx.globalAlpha = 0.055;
  ctx.fillStyle = '#087f5b';
  ctx.font = '800 68px Arial, sans-serif';
  ctx.fillText('SMALUX SPEEDTEST', 0, 0);
  ctx.restore();

  // 页脚记录影响结果可比性的关键任务参数和本地化测试时间。
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
    // Object URL 只在触发下载期间存活，随后释放以免多次导出积累浏览器内存。
    const link = document.createElement('a');
    link.href = URL.createObjectURL(blob);
    link.download = `smalux-speedtest-${taskID.slice(0, 8)}.png`;
    link.click();
    setTimeout(() => URL.revokeObjectURL(link.href), 1000);
  }, 'image/png');
}

// 将列宽转换为每列左边界，供热力格、文字和网格线复用同一套几何数据。
function columnOffsets(columns) {
  const offsets = [];
  let current = 0;
  columns.forEach(width => { offsets.push(current); current += width; });
  return offsets;
}

// 绘制按比例加深的背景；速度列额外在底部显示长度条，延迟列只使用色阶。
function heatCell(ctx, x, y, width, height, ratio, color, bar = false) {
  // API 异常值也被约束到 [0,1]，防止色块超出单元格。
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

// 在单元格裁剪区内居中绘字，确保过长名称不会覆盖相邻列。
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

// Canvas 不支持 CSS text-overflow；按实际字体测量结果逐字符截断并补省略号。
function trimCanvasText(ctx, value, maxWidth) {
  if (ctx.measureText(value).width <= maxWidth) return value;
  let output = value;
  while (output.length && ctx.measureText(`${output}…`).width > maxWidth) output = output.slice(0, -1);
  return `${output}…`;
}

// 图片中的时延固定保留一位小数，零值表示没有得到有效测量结果。
function formatDelay(value) { return value > 0 ? `${value.toFixed(1)} ms` : '—'; }

// 图片速率根据量级选择 Mbps/Gbps，同时保持服务端存储的 bit/s 语义。
function formatImageSpeed(value) {
  if (!value || value <= 0) return '—';
  return value >= 1_000_000_000 ? `${(value / 1_000_000_000).toFixed(2)} Gbps` : `${(value / 1_000_000).toFixed(2)} Mbps`;
}
