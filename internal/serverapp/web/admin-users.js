import { $, escapeHTML, showToast } from './ui.js';

// initAdminUsers owns the administrator table and dialog state. Keeping this
// controller separate prevents Dashboard task/client polling from accumulating
// account-management details and gives all mutations one refresh boundary.
export function initAdminUsers({api, dateText}) {
  const currentAdminID = document.querySelector('meta[name="current-admin-id"]').content;
  let admins = [];
  let loadInFlight = null;

  function render() {
    $('#admins-body').innerHTML = admins.length ? admins.map(item => {
      const current = item.id === currentAdminID;
      return `<tr>
        <td><span class="status ${item.enabled ? 'online' : ''}">${item.enabled ? '启用' : '停用'}</span></td>
        <td><strong>${escapeHTML(item.username)}</strong>${current ? ' <span class="tag">当前</span>' : ''}</td>
        <td>${dateText(item.last_login_at)}</td>
        <td>${dateText(item.created_at)}</td>
        <td><div class="row-actions">
          <button class="quiet admin-action" data-action="toggle" data-id="${escapeHTML(item.id)}" type="button" ${current ? 'disabled title="当前账户不能停用"' : ''}>${item.enabled ? '停用' : '启用'}</button>
          <button class="danger admin-action" data-action="delete" data-id="${escapeHTML(item.id)}" type="button" ${current ? 'disabled title="当前账户不能删除"' : ''}>删除</button>
        </div></td>
      </tr>`;
    }).join('') : '<tr><td colspan="5" class="empty">暂无管理员</td></tr>';
  }

  // Mutations wait for an older list request and then force a new snapshot, so a
  // slow periodic refresh cannot overwrite the state produced by the mutation.
  async function load({force = false} = {}) {
    if (loadInFlight) {
      if (!force) return loadInFlight;
      const activeRequest = loadInFlight;
      try { await activeRequest; } catch (_) { /* Continue with the required fresh request. */ }
      if (loadInFlight === activeRequest) loadInFlight = null;
      return load();
    }
    const request = api('/api/admin-users').then(items => {
      admins = Array.isArray(items) ? items : [];
      render();
      return admins;
    });
    loadInFlight = request;
    try { return await request; }
    finally { if (loadInFlight === request) loadInFlight = null; }
  }

  $('#new-admin').addEventListener('click', () => {
    $('#admin-error').classList.add('hidden');
    $('#admin-dialog').showModal();
  });
  document.querySelectorAll('.close-admin').forEach(button => button.addEventListener('click', () => $('#admin-dialog').close()));

  // The browser checks confirmation equality for fast feedback. The Server still
  // validates the one submitted password before hashing it with bcrypt.
  $('#admin-form').addEventListener('submit', async event => {
    event.preventDefault();
    const formElement = event.currentTarget;
    const form = new FormData(formElement);
    const errorBox = $('#admin-error');
    const submit = formElement.querySelector('button[type="submit"]');
    const password = String(form.get('password') || '');
    errorBox.classList.add('hidden');
    if (password !== String(form.get('password_confirm') || '')) {
      errorBox.textContent = '两次输入的密码不一致。';
      errorBox.classList.remove('hidden');
      return;
    }
    submit.disabled = true;
    submit.textContent = '创建中…';
    try {
      await api('/api/admin-users', {method:'POST', body:JSON.stringify({username:form.get('username'), password})});
      $('#admin-dialog').close();
      formElement.reset();
      await load({force: true});
      showToast('管理员已创建。', 'success');
    } catch (error) {
      errorBox.textContent = error.message;
      errorBox.classList.remove('hidden');
    } finally {
      submit.disabled = false;
      submit.textContent = '创建';
    }
  });

  // Disabling revokes all target sessions and deletion is permanent. The Server
  // independently protects the current and last enabled administrator.
  $('#admins-body').addEventListener('click', async event => {
    const button = event.target.closest('.admin-action');
    if (!button || button.disabled) return;
    const admin = admins.find(item => item.id === button.dataset.id);
    if (!admin) return;
    const deleting = button.dataset.action === 'delete';
    if (deleting && !confirm(`确认永久删除管理员 ${admin.username}？`)) return;
    if (!deleting && admin.enabled && !confirm(`确认停用管理员 ${admin.username}？其现有会话将失效。`)) return;
    button.disabled = true;
    try {
      if (deleting) await api(`/api/admin-users/${button.dataset.id}`, {method:'DELETE'});
      else await api(`/api/admin-users/${button.dataset.id}`, {method:'PATCH', body:JSON.stringify({enabled:!admin.enabled})});
      await load({force: true});
      showToast(deleting ? '管理员已删除。' : (admin.enabled ? '管理员已停用。' : '管理员已启用。'), 'success');
    } catch (error) {
      button.disabled = false;
      showToast(error.message, 'error');
    }
  });

  return {load};
}
