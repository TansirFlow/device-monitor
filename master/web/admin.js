/* 管理后台前端。零依赖：只有 fetch + DOM，与看板同一套样式变量。
   所有写操作都带 X-Mon-Csrf 头 —— 后端要求它，浏览器表单伪造不了请求头。 */

'use strict';

var CSRF = { 'X-Mon-Csrf': '1' };
var state = { nodes: [], masterUrl: '', agentReady: false, loggedIn: false };

// ---------------------------------------------------------------- 工具

function $(id) { return document.getElementById(id); }

function show(id) { $(id).classList.remove('hidden'); }
function hide(id) { $(id).classList.add('hidden'); }

function err(msg) {
  var el = $('error');
  if (!msg) { hide('error'); return; }
  el.textContent = msg;
  show('error');
}

function toast(msg) {
  var el = $('toast');
  el.textContent = msg;
  show('toast');
  setTimeout(function () { hide('toast'); }, 4000);
}

function esc(s) {
  return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
    return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
  });
}

function api(path, opts) {
  opts = opts || {};
  var headers = Object.assign({}, opts.headers || {});
  if (opts.method && opts.method !== 'GET') {
    headers = Object.assign(headers, CSRF, { 'Content-Type': 'application/json' });
  }
  return fetch(path, {
    method: opts.method || 'GET',
    headers: headers,
    body: opts.body ? JSON.stringify(opts.body) : undefined,
    credentials: 'same-origin'
  }).then(function (r) {
    return r.json().catch(function () { return {}; }).then(function (data) {
      if (!r.ok) {
        var e = new Error(data && data.error ? data.error : ('请求失败（HTTP ' + r.status + '）'));
        e.status = r.status;
        throw e;
      }
      return data;
    });
  });
}

// ---------------------------------------------------------------- 登录态

function applyTheme() {
  // 与看板共用同一个键（mon_theme），两边切主题保持一致
  var t = null;
  try { t = localStorage.getItem('mon_theme'); } catch (e) { /* 隐私模式下忽略 */ }
  if (t === 'dark' || t === 'light') {
    document.documentElement.setAttribute('data-theme', t);
  }
}

function renderShell() {
  if (state.loggedIn) {
    hide('loginView');
    show('panelView');
  } else {
    show('loginView');
    hide('panelView');
    $('whoami').textContent = '';
  }
}

function boot() {
  applyTheme();
  return api('/api/v1/admin/state').then(function (s) {
    state.loggedIn = !!s.logged_in;
    renderShell();
    if (state.loggedIn) return loadNodes();
  }).catch(function (e) { err(e.message); });
}

// ---------------------------------------------------------------- 节点列表

function loadNodes() {
  return api('/api/v1/admin/nodes').then(function (d) {
    state.nodes = d.nodes || [];
    state.masterUrl = d.master_url || location.origin;
    state.agentReady = !!d.agent_dir_ready;
    renderNodes();
  });
}

function renderNodes() {
  var list = $('nodeList');
  list.innerHTML = '';
  $('emptyHint').classList.toggle('hidden', state.nodes.length > 0);

  $('agentHint').innerHTML = state.agentReady
    ? '主控已配置客户端二进制目录，安装脚本可直接下载 agent。'
    : '主控<b>未配置</b> <code>agent_dir</code>：安装脚本会生成，但二进制下载会 404。把 dist/ 里的二进制放到某个目录后，在 <code>master.json</code> 里设 <code>agent_dir</code> 并重启主控。';

  state.nodes.forEach(function (n) {
    var row = document.createElement('div');
    row.className = 'admin-row' + (n.disabled ? ' is-disabled' : '');

    var tags = [];
    if (n.disabled) tags.push('<span class="tag lv-crit">已停用</span>');
    else if (n.last_seen > 0) tags.push('<span class="tag lv-ok">在用</span>');
    else tags.push('<span class="tag">未上报</span>');
    if (n.source === 'config') tags.push('<span class="tag" title="来自 master.json，后台未接管；动过它之后就由 nodes.json 管理">来自配置</span>');
    if (!n.has_install_code) tags.push('<span class="tag lv-warn">无安装码</span>');

    row.innerHTML =
      '<div class="admin-main-col">' +
        '<div class="admin-name">' + esc(n.node_id) + ' ' + tags.join(' ') + '</div>' +
        '<div class="muted small">' +
          '最后上报：' + esc(n.last_seen_ago || '从未上报') +
          (n.note ? ' · 备注：' + esc(n.note) : '') +
        '</div>' +
        '<div class="admin-token">' +
          '<span class="muted small">令牌：</span>' +
          '<code class="token" data-token="' + esc(n.token) + '">••••••••••••</code>' +
          '<button class="icon-btn tiny toggle-token">显示</button>' +
        '</div>' +
      '</div>' +
      '<div class="admin-ops">' +
        '<button class="icon-btn" data-act="install">安装</button>' +
        '<button class="icon-btn" data-act="token">轮换令牌</button>' +
        '<button class="icon-btn" data-act="code">轮换安装码</button>' +
        '<button class="icon-btn" data-act="revoke">撤销安装码</button>' +
        '<button class="icon-btn" data-act="toggle">' + (n.disabled ? '启用' : '停用') + '</button>' +
        '<button class="icon-btn danger" data-act="del">删除</button>' +
      '</div>';

    row.querySelectorAll('[data-act]').forEach(function (btn) {
      btn.addEventListener('click', function () { onNodeAction(btn.getAttribute('data-act'), n); });
    });
    var toggle = row.querySelector('.toggle-token');
    toggle.addEventListener('click', function () {
      var code = row.querySelector('.token');
      var shown = code.textContent !== '••••••••••••';
      code.textContent = shown ? '••••••••••••' : code.getAttribute('data-token');
      toggle.textContent = shown ? '显示' : '隐藏';
    });
    list.appendChild(row);
  });
}

function onNodeAction(act, n) {
  err('');
  var id = n.node_id;
  if (act === 'install') {
    api('/api/v1/admin/nodes/' + encodeURIComponent(id) + '/code', { method: 'POST', body: {} })
      .then(function (d) { showInstall(id, d.install, n.token); })
      .then(loadNodes)
      .catch(function (e) { err(e.message); });
    return;
  }
  if (act === 'token') {
    if (!confirm('轮换 ' + id + ' 的上报令牌？\n\n已安装的节点需要重新执行安装脚本（或手工改配置后重启服务），否则会开始上报失败。')) return;
    api('/api/v1/admin/nodes/' + encodeURIComponent(id) + '/token', { method: 'POST', body: {} })
      .then(function (d) {
        toast('已生成新令牌：' + d.token);
        showInstall(id, d.install, d.token);
      })
      .then(loadNodes)
      .catch(function (e) { err(e.message); });
    return;
  }
  if (act === 'code') {
    api('/api/v1/admin/nodes/' + encodeURIComponent(id) + '/code', { method: 'POST', body: {} })
      .then(function (d) { showInstall(id, d.install, n.token); })
      .then(loadNodes)
      .catch(function (e) { err(e.message); });
    return;
  }
  if (act === 'revoke') {
    api('/api/v1/admin/nodes/' + encodeURIComponent(id) + '/code', { method: 'DELETE' })
      .then(function () { toast('安装命令已撤销'); })
      .then(loadNodes)
      .catch(function (e) { err(e.message); });
    return;
  }
  if (act === 'toggle') {
    api('/api/v1/admin/nodes/' + encodeURIComponent(id) + '/disabled', { method: 'POST', body: { disabled: !n.disabled } })
      .then(function () { toast(n.disabled ? '已启用 ' + id : '已停用 ' + id); })
      .then(loadNodes)
      .catch(function (e) { err(e.message); });
    return;
  }
  if (act === 'del') {
    if (!confirm('删除节点 ' + id + '？\n\n只会移除主控这里的令牌记录（不影响已安装机器上的配置）。若该节点来自 master.json，重启后还会重新出现。')) return;
    api('/api/v1/admin/nodes/' + encodeURIComponent(id), { method: 'DELETE' })
      .then(function () { toast('已删除 ' + id); })
      .then(loadNodes)
      .catch(function (e) { err(e.message); });
  }
}

// ---------------------------------------------------------------- 安装命令

function showInstall(nodeId, install, token) {
  if (!install || !install.ready) {
    err(install && install.hint ? install.hint : '该节点还没有可用的安装码');
    return;
  }
  $('cmdTitle').textContent = '一键安装 · ' + nodeId;
  $('cmdLinux').textContent = install.linux || '';
  $('cmdWin').textContent = install.windows || '';
  $('cmdUrls').textContent = (install.sh_url || '') + '\n' + (install.ps1_url || '');
  $('cmdToken').textContent = token || '（点击「轮换令牌」后可见新令牌）';
  show('cmdModal');
}

// ---------------------------------------------------------------- 表单

function wireForms() {
  $('loginForm').addEventListener('submit', function (e) {
    e.preventDefault();
    err('');
    var u = $('loginUser').value.trim();
    var p = $('loginPass').value;
    if (!u || !p) { err('请填写用户名与密码'); return; }
    $('loginBtn').disabled = true;
    api('/api/v1/admin/login', { method: 'POST', body: { username: u, password: p } })
      .then(function () {
        state.loggedIn = true;
        $('loginPass').value = '';
        $('whoami').textContent = u;
        renderShell();
        return loadNodes();
      })
      .catch(function (e) { err(e.message); })
      .then(function () { $('loginBtn').disabled = false; });
  });

  $('logoutBtn').addEventListener('click', function () {
    api('/api/v1/admin/logout', { method: 'POST', body: {} })
      .catch(function () { /* 退出失败也要回到登录页 */ })
      .then(function () {
        state.loggedIn = false;
        state.nodes = [];
        renderShell();
        err('');
      });
  });

  $('pwdBtn').addEventListener('click', function () {
    $('oldPass').value = ''; $('newPass').value = ''; $('newPass2').value = '';
    hide('pwdErr');
    show('pwdModal');
  });
  $('pwdCancel').addEventListener('click', function () { hide('pwdModal'); });
  $('pwdSave').addEventListener('click', function () {
    var oldP = $('oldPass').value, np = $('newPass').value, np2 = $('newPass2').value;
    if (np !== np2) { $('pwdErr').textContent = '两次输入的新密码不一致'; show('pwdErr'); return; }
    api('/api/v1/admin/password', { method: 'POST', body: { old_password: oldP, new_password: np } })
      .then(function () {
        hide('pwdModal');
        state.loggedIn = false;
        renderShell();
        toast('密码已修改，请用新密码重新登录');
      })
      .catch(function (e) { $('pwdErr').textContent = e.message; show('pwdErr'); });
  });

  $('newNodeBtn').addEventListener('click', function () {
    $('newNodeId').value = ''; $('newToken').value = ''; $('newNote').value = '';
    hide('newErr');
    show('newModal');
  });
  $('newCancel').addEventListener('click', function () { hide('newModal'); });
  $('newSave').addEventListener('click', function () {
    var id = $('newNodeId').value.trim();
    if (!id) { $('newErr').textContent = '请填写节点名'; show('newErr'); return; }
    api('/api/v1/admin/nodes', {
      method: 'POST',
      body: { node_id: id, token: $('newToken').value.trim(), note: $('newNote').value.trim() }
    }).then(function (d) {
      hide('newModal');
      toast('节点 ' + id + ' 已创建');
      showInstall(id, d.install, d.node && d.node.token);
      return loadNodes();
    }).catch(function (e) { $('newErr').textContent = e.message; show('newErr'); });
  });

  $('cmdClose').addEventListener('click', function () { hide('cmdModal'); });

  document.querySelectorAll('.copy-btn').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var target = $(btn.getAttribute('data-copy'));
      copyText(target.textContent);
      toast('已复制');
    });
  });

  // 点遮罩关闭弹窗
  document.querySelectorAll('.modal').forEach(function (m) {
    m.addEventListener('click', function (e) { if (e.target === m) m.classList.add('hidden'); });
  });
  document.addEventListener('keydown', function (e) {
    if (e.key === 'Escape') document.querySelectorAll('.modal').forEach(function (m) { m.classList.add('hidden'); });
  });
}

function copyText(text) {
  if (navigator.clipboard && navigator.clipboard.writeText) {
    navigator.clipboard.writeText(text).catch(function () { fallbackCopy(text); });
    return;
  }
  fallbackCopy(text);
}

function fallbackCopy(text) {
  var ta = document.createElement('textarea');
  ta.value = text;
  ta.style.position = 'fixed';
  ta.style.opacity = '0';
  document.body.appendChild(ta);
  ta.select();
  try { document.execCommand('copy'); } catch (e) { /* 浏览器不支持就算了 */ }
  document.body.removeChild(ta);
}

wireForms();
boot();
