import { $, copyText, escapeHTML, showToast } from './ui.js';

// initAdminUsers owns the administrator table and dialog state. Keeping this
// controller separate prevents Dashboard task/client polling from accumulating
// account-management details and gives all mutations one refresh boundary.
export function initAdminUsers({api, dateText}) {
  if (document.querySelector('meta[name="current-admin-owner"]')?.content !== 'true') return {load: async () => []};
  const currentAdminID = document.querySelector('meta[name="current-admin-id"]').content;
  let admins = [];
  let invites = [];
  let loadInFlight = null;
  let invitesLoadInFlight = null;
  let currentInviteCode = '';
  let currentInviteLink = '';

  function render() {
    $('#admins-body').innerHTML = admins.length ? admins.map(item => {
      const current = item.id === currentAdminID;
      return `<tr>
        <td><span class="status ${item.enabled ? 'online' : ''}">${item.enabled ? '启用' : '停用'}</span></td>
        <td><strong>${escapeHTML(item.username)}</strong>${item.is_owner ? ' <span class="tag owner">最高权限</span>' : (current ? ' <span class="tag">当前</span>' : '')}</td>
        <td>${dateText(item.last_login_at)}</td>
        <td>${dateText(item.created_at)}</td>
        <td><div class="row-actions">
          <button class="quiet admin-action" data-action="toggle" data-id="${escapeHTML(item.id)}" type="button" ${current ? 'disabled title="当前账户不能停用"' : ''}>${item.enabled ? '停用' : '启用'}</button>
          <button class="danger admin-action" data-action="delete" data-id="${escapeHTML(item.id)}" type="button" ${current ? 'disabled title="当前账户不能删除"' : ''}>删除</button>
        </div></td>
      </tr>`;
    }).join('') : '<tr><td colspan="5" class="empty">暂无管理员</td></tr>';
  }

  function inviteStatus(invite) {
    if (invite.used_at) return {className: 'completed', text: '已使用'};
    if (invite.revoked_at) return {className: 'failed', text: '已吊销'};
    return {className: 'online', text: '可注册'};
  }

  function renderInvites() {
    $('#invite-count').textContent = `${invites.length} 个邀请`;
    $('#invites-body').innerHTML = invites.length ? invites.map(invite => {
      const status = inviteStatus(invite);
      const active = !invite.used_at && !invite.revoked_at;
      return `<tr>
        <td><span class="status ${status.className}">${status.text}</span></td>
        <td>${dateText(invite.created_at)}</td>
        <td>${dateText(invite.used_at)}</td>
        <td><div class="row-actions">${active ? `<button class="danger invite-action" data-id="${escapeHTML(invite.id)}" type="button">吊销</button>` : '<span class="muted">不可操作</span>'}</div></td>
      </tr>`;
    }).join('') : '<tr><td colspan="4" class="empty">暂无邀请</td></tr>';
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

  async function loadInvites({force = false} = {}) {
    if (invitesLoadInFlight) {
      if (!force) return invitesLoadInFlight;
      const activeRequest = invitesLoadInFlight;
      try { await activeRequest; } catch (_) { /* Continue with the required fresh request. */ }
      if (invitesLoadInFlight === activeRequest) invitesLoadInFlight = null;
      return loadInvites();
    }
    const request = api('/api/admin-invites').then(items => {
      invites = Array.isArray(items) ? items : [];
      renderInvites();
      return invites;
    });
    invitesLoadInFlight = request;
    try { return await request; }
    finally { if (invitesLoadInFlight === request) invitesLoadInFlight = null; }
  }

  $('#new-admin').addEventListener('click', async event => {
    const button = event.currentTarget;
    const errorBox = $('#invite-error');
    errorBox.classList.add('hidden');
    button.disabled = true;
    button.textContent = '生成中…';
    try {
      const result = await api('/api/admin-invites', {method:'POST'});
      currentInviteCode = result.code || '';
      currentInviteLink = `${window.location.origin}/register?invite=${encodeURIComponent(currentInviteCode)}`;
      $('#invite-code-output').value = currentInviteCode;
      $('#invite-link-output').value = currentInviteLink;
      $('#copy-invite').textContent = '复制邀请码';
      $('#copy-invite-link').textContent = '复制链接';
      $('#invite-dialog').showModal();
      await loadInvites({force: true});
      showToast('邀请已生成。', 'success', 6000);
    } catch (error) {
      showToast(error.message, 'error', 6000);
    } finally {
      button.disabled = false;
      button.textContent = '生成邀请';
    }
  });
  document.querySelectorAll('.close-invite').forEach(button => button.addEventListener('click', () => $('#invite-dialog').close()));

  $('#copy-invite').addEventListener('click', async () => {
    try {
      await copyText(currentInviteCode);
      $('#copy-invite').textContent = '已复制';
      showToast('邀请码已复制。', 'success');
    } catch (_) { showToast('浏览器拒绝访问剪贴板，请手动复制。', 'error'); }
  });

  $('#copy-invite-link').addEventListener('click', async () => {
    try {
      await copyText(currentInviteLink);
      $('#copy-invite-link').textContent = '已复制';
      showToast('注册链接已复制。', 'success');
    } catch (_) { showToast('浏览器拒绝访问剪贴板，请手动复制。', 'error'); }
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

  $('#invites-body').addEventListener('click', async event => {
    const button = event.target.closest('.invite-action');
    if (!button || button.disabled) return;
    if (!confirm('确认吊销这个注册邀请？')) return;
    button.disabled = true;
    try {
      await api(`/api/admin-invites/${button.dataset.id}`, {method:'DELETE'});
      await loadInvites({force: true});
      showToast('邀请已吊销。', 'success');
    } catch (error) {
      button.disabled = false;
      showToast(error.message, 'error');
    }
  });

  return {load: async () => Promise.all([load(), loadInvites()])};
}
