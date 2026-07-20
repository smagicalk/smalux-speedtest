// ui.js 保存管理端页面共享的、与具体业务数据无关的浏览器交互工具。
// 文件保持无状态，Dashboard 和任务详情可以独立加载并复用同一套安全边界。
export const $ = selector => document.querySelector(selector);

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
