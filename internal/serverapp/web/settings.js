import { $, redirectToLogin, showToast } from './ui.js';
import { initAdminUsers } from './admin-users.js';

const csrf = document.querySelector('meta[name="csrf-token"]').content;
let telegram = null;
let challengeID = '';

const api = async (url, options = {}) => {
  const headers = { ...(options.headers || {}) };
  if (options.body) headers['Content-Type'] = 'application/json';
  if (options.method && options.method !== 'GET') headers['X-CSRF-Token'] = csrf;
  const response = await fetch(url, { ...options, headers });
  if (redirectToLogin(response)) throw new Error('登录已失效');
  let data = null;
  try { data = response.status === 204 ? null : await response.json(); } catch (_) { /* 统一错误处理。 */ }
  if (!response.ok) throw new Error(data?.error || `请求失败（${response.status}）`);
  return data;
};

const dateText = value => value ? new Date(value).toLocaleString() : '—';
const adminUsers = initAdminUsers({api, dateText});

function setTelegramStatus(state, text) {
  const target = $('#telegram-status');
  target.className = `sync-status ${state}`;
  target.textContent = text;
}

function renderTelegram() {
  const configured = Boolean(telegram?.configured);
  $('#telegram-bound').classList.toggle('hidden', !configured);
  $('#telegram-bind-form').classList.toggle('hidden', configured);
  if (!configured) {
    setTelegramStatus('', '未绑定');
    return;
  }
  $('#telegram-bot').textContent = telegram.bot_username ? `@${telegram.bot_username}` : String(telegram.bot_id || '—');
  $('#telegram-owner').textContent = telegram.owner_telegram_id || '—';
  $('#telegram-runtime').textContent = telegram.running ? '运行中' : (telegram.enabled ? '启动异常' : '已关闭');
  $('#telegram-enabled').checked = Boolean(telegram.enabled);
  setTelegramStatus(telegram.running ? 'success' : '', telegram.running ? '运行中' : '已停止');
}

async function loadTelegram() {
  telegram = await api('/api/settings/telegram');
  renderTelegram();
  return telegram;
}

$('#telegram-bind-form').addEventListener('submit', async event => {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const errorBox = $('#telegram-bind-error');
  const submit = formElement.querySelector('button[type="submit"]');
  errorBox.classList.add('hidden');
  submit.disabled = true;
  submit.textContent = '验证中…';
  try {
    const result = await api('/api/settings/telegram/verify', {
      method: 'POST',
      body: JSON.stringify({token: form.get('token'), owner_id: Number(form.get('owner_id'))}),
    });
    challengeID = result.challenge_id;
    $('#telegram-bind-form').classList.add('hidden');
    $('#telegram-confirm-form').classList.remove('hidden');
    $('#telegram-verify-target').textContent = `验证码已发送到 ${result.bot_username ? `@${result.bot_username}` : 'Bot'} 对应会话`;
    $('#telegram-confirm-form input[name="code"]').focus();
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally {
    submit.disabled = false;
    submit.textContent = '发送验证码';
  }
});

$('#telegram-confirm-form').addEventListener('submit', async event => {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const errorBox = $('#telegram-confirm-error');
  const submit = formElement.querySelector('button[type="submit"]');
  errorBox.classList.add('hidden');
  submit.disabled = true;
  submit.textContent = '绑定中…';
  try {
    telegram = await api('/api/settings/telegram/confirm', {
      method: 'POST', body: JSON.stringify({challenge_id: challengeID, code: form.get('code')}),
    });
    challengeID = '';
    formElement.reset();
    $('#telegram-bind-form').reset();
    $('#telegram-confirm-form').classList.add('hidden');
    renderTelegram();
    showToast('Telegram Bot 已绑定并启动。', 'success');
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
  } finally {
    submit.disabled = false;
    submit.textContent = '确认绑定';
  }
});

$('#telegram-cancel-verify').addEventListener('click', () => {
  challengeID = '';
  $('#telegram-confirm-form').reset();
  $('#telegram-confirm-form').classList.add('hidden');
  $('#telegram-bind-form').classList.remove('hidden');
});

$('#telegram-enabled').addEventListener('change', async event => {
  const input = event.currentTarget;
  const enabled = input.checked;
  input.disabled = true;
  try {
    await api('/api/settings/telegram', {method:'PATCH', body:JSON.stringify({enabled})});
    await loadTelegram();
    showToast(enabled ? 'Bot 已启动。' : 'Bot 已停止。', 'success');
  } catch (error) {
    input.checked = !enabled;
    showToast(error.message, 'error', 6000);
  } finally {
    input.disabled = false;
  }
});

$('#telegram-rebind').addEventListener('click', () => {
  $('#telegram-bind-error').classList.add('hidden');
  $('#telegram-bind-form').classList.remove('hidden');
  $('#telegram-bind-form input[name="token"]').focus();
});

$('#telegram-unbind').addEventListener('click', async event => {
  if (!confirm('确认解除 Telegram Bot 绑定？')) return;
  const button = event.currentTarget;
  button.disabled = true;
  try {
    await api('/api/settings/telegram', {method:'DELETE'});
    telegram = {configured:false, enabled:false, running:false};
    $('#telegram-bind-form').reset();
    renderTelegram();
    showToast('Telegram Bot 已解除绑定。', 'success');
  } catch (error) {
    showToast(error.message, 'error', 6000);
  } finally {
    button.disabled = false;
  }
});

Promise.all([loadTelegram(), adminUsers.load()]).catch(error => {
  setTelegramStatus('error', '加载失败');
  showToast(error.message, 'error', 6000);
});
