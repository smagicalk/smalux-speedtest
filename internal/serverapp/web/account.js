import { $, redirectToLogin, showToast } from './ui.js';

const csrf = document.querySelector('meta[name="csrf-token"]').content;

$('#password-form').addEventListener('submit', async event => {
  event.preventDefault();
  const formElement = event.currentTarget;
  const form = new FormData(formElement);
  const errorBox = $('#password-error');
  const submit = formElement.querySelector('button[type="submit"]');
  const newPassword = String(form.get('new_password') || '');
  const confirmation = String(form.get('new_password_confirm') || '');

  errorBox.classList.add('hidden');
  if (newPassword !== confirmation) {
    errorBox.textContent = '两次输入的新密码不一致';
    errorBox.classList.remove('hidden');
    return;
  }

  submit.disabled = true;
  submit.textContent = '保存中…';
  try {
    const response = await fetch('/api/account/password', {
      method: 'POST',
      headers: {'Content-Type': 'application/json', 'X-CSRF-Token': csrf},
      body: JSON.stringify({
        current_password: form.get('current_password'),
        new_password: newPassword,
        new_password_confirm: confirmation,
      }),
    });
    if (redirectToLogin(response)) throw new Error('登录已失效');
    let data = null;
    try { data = await response.json(); } catch (_) { /* 使用状态码兜底。 */ }
    if (!response.ok) throw new Error(data?.error || `请求失败（${response.status}）`);
    formElement.reset();
    showToast('密码已修改，正在返回控制台。', 'success', 1800);
    window.setTimeout(() => window.location.assign('/'), 900);
  } catch (error) {
    errorBox.textContent = error.message;
    errorBox.classList.remove('hidden');
    submit.disabled = false;
    submit.textContent = '修改密码';
  }
});
