// ui.js 保存管理端页面共享的、与具体业务数据无关的浏览器交互工具。
// 文件保持无状态，Dashboard 和任务详情可以独立加载并复用同一套安全边界。
export const $ = selector => document.querySelector(selector);

// REST requests use a finite deadline so a stalled connection cannot leave controls
// permanently disabled. EventSource manages its own reconnect lifecycle separately.
export async function fetchWithTimeout(input, options = {}, timeoutMS = 12000) {
  const controller = new AbortController();
  const upstreamSignal = options.signal;
  const abortFromUpstream = () => controller.abort(upstreamSignal.reason);
  if (upstreamSignal) {
    if (upstreamSignal.aborted) abortFromUpstream();
    else upstreamSignal.addEventListener('abort', abortFromUpstream, {once:true});
  }
  const timer = window.setTimeout(() => controller.abort(), timeoutMS);
  try {
    return await fetch(input, {...options, signal:controller.signal});
  } catch (error) {
    if (controller.signal.aborted && !upstreamSignal?.aborted) throw new Error('请求超时，请重试');
    throw error;
  } finally {
    window.clearTimeout(timer);
    upstreamSignal?.removeEventListener('abort', abortFromUpstream);
  }
}

// redirectToLogin 统一处理 API 的 JSON 401 和被浏览器跟随后的登录页重定向。
// 返回 true 让调用方立即停止解析当前响应，避免把登录 HTML 当成业务 JSON。
export function redirectToLogin(response) {
  let loginRedirect = false;
  if (response.redirected) {
    try { loginRedirect = new URL(response.url, window.location.href).pathname === '/login'; } catch (_) { /* 使用状态码兜底。 */ }
  }
  if (response.status !== 401 && !loginRedirect) return false;
  if (window.location.pathname !== '/login') window.location.assign('/login');
  return true;
}

// showToast 提供不阻塞当前操作的统一反馈；错误使用 alert 语义，其余消息使用 status。
export function showToast(message, type = 'info', duration = 4000) {
  const region = $('#toast-region');
  if (!region) return;
  const toast = document.createElement('div');
  toast.className = `toast ${type}`;
  toast.setAttribute('role', type === 'error' ? 'alert' : 'status');
  toast.textContent = message;
  region.append(toast);
  window.setTimeout(() => toast.remove(), duration);
}

// escapeHTML 用于批量 innerHTML 渲染；任何来自 API 或代理名称的文本都必须先经过它。
export function escapeHTML(value) {
  return String(value ?? '').replace(/[&<>'"]/g, character => ({'&':'&amp;','<':'&lt;','>':'&gt;',"'":'&#39;','"':'&quot;'}[character]));
}

export function statusText(value) {
  return {queued:'排队中',running:'进行中',completed:'完成',partial:'部分完成',failed:'失败',canceled:'已取消'}[value] || value;
}

// Task and target details are constrained by the Store to this fixed vocabulary.
// Translate known categories while preserving a bounded server fallback.
export function taskDetailText(value) {
  return {
    'task timed out':'任务超时',
    'task canceled':'任务已取消',
    'client task failed':'Client 执行失败',
    'all clients failed':'全部 Client 执行失败',
    'some clients did not complete':'部分 Client 未完成',
    'server restarted before task completed':'Server 重启导致任务中断',
    'task expired after 10 minutes':'任务等待超过 10 分钟',
    'canceled by administrator':'管理员已取消任务',
    'client token revoked':'Client Token 已吊销',
    'client disconnected':'Client 已断开',
    'client connection replaced':'Client 连接已被替换',
    'finished':'已完成',
  }[value] || value;
}

export function resultErrorText(value) {
  return {
    'proxy initialization failed':'代理初始化失败',
    'speedtest server discovery failed':'测速节点发现失败',
    'no speedtest server available':'没有可用测速节点',
    'all speedtest latency checks failed':'测速节点延迟检测全部失败',
    'download test failed':'下载测试失败',
    'upload test failed':'上传测试失败',
    'download and upload tests failed':'下载和上传测试均失败',
    'proxy test failed':'代理测试失败',
  }[value] || value;
}

// copyText 优先使用 Clipboard API；不支持时退回到临时 textarea，兼容旧浏览器和
// 部分非安全上下文。调用方负责展示成功或失败提示。
export async function copyText(value) {
  if (navigator.clipboard?.writeText) return navigator.clipboard.writeText(value);
  const helper = document.createElement('textarea');
  helper.value = value;
  helper.style.position = 'fixed';
  helper.style.opacity = '0';
  document.body.append(helper);
  helper.select();
  document.execCommand('copy');
  helper.remove();
}
