'use strict';
const el = id => document.getElementById(id);
const make = (tag, text) => {
  const e = document.createElement(tag);
  if (text !== undefined) e.textContent = text;
  return e;
};
function button(title, fn) {
  const b = make('button', title);
  b.type = 'button';
  b.onclick = () => tryRun(fn);
  return b;
}
function notice(message) {
  el('notice').textContent = message;
}
async function tryRun(fn) {
  try {
    await fn();
  } catch (e) {
    notice(e.message);
    if (el('dialog').open) {
      const p = make('p', e.message);
      p.setAttribute('role', 'alert');
      el('dialog-body').append(p);
    }
  }
}
async function api(path, body) {
  const init = body === undefined ? {} : {
    method: 'POST',
    headers: {'Content-Type': 'application/json', 'X-PTO-Request': '1'},
    body: JSON.stringify(body)
  };
  const response = await fetch('/api/' + path, init);
  const text = await response.text();
  let data;
  try {
    data = JSON.parse(text);
  } catch {
    data = {error: text};
  }
  if (!response.ok) throw Error(data.error || response.statusText);
  return data;
}
const labels = {
  probe: '刷新盘点',
  'read-config': '读取配置',
  'apply-config': '下发配置',
  update: '更新版本',
  install: '首次安装',
  activate: '激活版本',
  rollback: '回退版本'
};
const statuses = {
  queued: '等待派发',
  dispatched: '等待回执',
  succeeded: '成功',
  failed: '失败',
  skipped: '跳过',
  conflict: '配置冲突',
  'pending-activation': '待激活',
  unknown: '待核对',
  superseded: '已替代',
  progress: '执行中'
};
const presence = {
  present: '已安装',
  absent: '未安装',
  unknown: '无法判定'
};
let state, tool, action, chosen = new Set(), fields = [];
function table(target, head, rows) {
  target.replaceChildren();
  const header = target.createTHead().insertRow();
  head.forEach(h => header.append(make('th', h)));
  const body = target.createTBody();
  for (const cells of rows) {
    const tr = body.insertRow();
    cells.forEach(v => {
      const td = tr.insertCell();
      if (v instanceof Node)
        td.append(v);
      else
        td.textContent = v;
    });
  }
}
function observation(name, id) {
  return state.inventory.find(i => i.node === name && i.tool === id);
}
function modal(title) {
  el('dialog-title').textContent = title;
  el('dialog-body').replaceChildren();
  if (!el('dialog').open) el('dialog').showModal();
  return el('dialog-body');
}
function display(v) {
  return v === undefined    ? '未设置' :
      typeof v === 'string' ? v :
                              JSON.stringify(v);
}
function showTab() {
  let tab = location.hash.slice(1);
  if (!['fleet', 'tools', 'history'].includes(tab)) tab = 'fleet';
  for (const id of ['fleet', 'tools', 'history']) el(id).hidden = id !== tab;
  document.querySelectorAll('nav a').forEach(a => {
    if (a.hash === '#' + tab)
      a.setAttribute('aria-current', 'page');
    else
      a.removeAttribute('aria-current');
  });
}
function overview() {
  const ids = [...new Set(state.tools.map(t => t.manifest.id))];
  el('metrics').replaceChildren();
  for (const [name, n] of [
           ['在线节点', state.nodes.filter(n => n.online).length],
           ['已登记节点', state.nodes.length], ['已注册工具', ids.length]]) {
    const card = make('div');
    card.className = 'metric';
    card.append(make('span', name), make('strong', n));
    el('metrics').append(card);
  }
  const rows = state.nodes.map(
      n =>
          [n.name, n.group || '—', n.online ? '在线' : '离线',
           ...ids.map(id => {
             const o = observation(n.name, id);
             if (!o) return '等待盘点';
             if (o.presence !== 'present')
               return presence[o.presence] || '未知';
             const revision = o.writtenRevision ?
                 ' · 配置写入 ' + o.writtenRevision + ' / 生效 ' +
                     (o.effectiveRevision || '待核实') :
                 '';
             return '安装 ' + (o.installedVersion || '未知') + ' / 运行 ' +
                 (o.runningVersion || '未知') + revision;
           })]);
  table(el('matrix'), ['节点', '分组', '连接', ...ids], rows);
  table(
      el('history-table'),
      ['节点 / 工具', '操作', '状态', '结果', '操作人 / 时间'],
      state.tasks.map(t => {
        const details = make('details');
        details.append(
            make('summary', t.result?.summary || '详情'),
            make('pre', JSON.stringify(t, null, 2)));
        return [
          t.node + ' / ' + t.tool, labels[t.action] || t.action,
          statuses[t.state] || t.state, details,
          t.actor + ' · ' + new Date(t.created * 1000).toLocaleString()
        ];
      }));
}
async function refresh(reset = false) {
  state = await api('state');
  overview();
  document.querySelectorAll('.admin').forEach(
      e => e.hidden = state.role !== 'admin');
  const old = el('tool').value;
  el('tool').replaceChildren();
  for (const t of state.tools) {
    const opt = make('option', t.manifest.id + ' · ' + t.version);
    opt.value = t.digest;
    el('tool').append(opt);
  }
  if (state.tools.some(t => t.digest === old)) el('tool').value = old;
  if (reset || tool?.digest !== el('tool').value)
    chooseTool();
  else
    targets();
}
function chooseTool() {
  tool = state.tools.find(t => t.digest === el('tool').value);
  el('tool-empty').hidden = !!tool;
  el('operations').hidden = !tool;
  if (!tool) return;
  chosen.clear();
  action = null;
  el('actions').replaceChildren();
  const all = [
    ...tool.manifest.capabilities.filter(x => x !== 'validate-config')
        .map(id => ({
               id,
               title: labels[id],
               supportsBatch: true,
               effect: ['probe', 'read-config'].includes(id) ? 'read' : 'change'
             })),
    ...(tool.manifest.actions || [])
  ];
  for (const a of all) {
    if (a.effect === 'change' && state.role !== 'admin') continue;
    const b = button(a.title, () => selectAction(a, b));
    el('actions').append(b);
    if (!action) selectAction(a, b);
  }
  targets();
}
function matches(n) {
  const q = el('filter').value.toLowerCase();
  return (n.name + ' ' + (n.group || '')).toLowerCase().includes(q);
}
function eligible(n) {
  if (!n.enrolled) return false;
  if (!action || action.id === 'probe') return true;
  const o = observation(n.name, tool.manifest.id);
  if (action.id === 'install') return o?.presence === 'absent';
  return o?.presence === 'present' &&
      (!o.capabilities || o.capabilities.includes(action.id));
}
function targets() {
  if (!tool) return;
  el('targets').replaceChildren();
  for (const n of state.nodes.filter(matches)) {
    const label = make('label'), cb = make('input');
    cb.type = 'checkbox';
    cb.checked = chosen.has(n.name);
    cb.disabled = !eligible(n);
    if (cb.disabled) {
      chosen.delete(n.name);
      cb.checked = false;
    }
    cb.onchange = () => {
      if (cb.checked)
        chosen.add(n.name);
      else
        chosen.delete(n.name);
      form();
    };
    label.append(
        cb, document.createTextNode(n.name + (n.online ? '' : ' · 离线')));
    el('targets').append(label);
  }
}
function selectAction(a, b) {
  action = a;
  for (const n of state.nodes) {
    if (!eligible(n)) chosen.delete(n.name);
  }
  el('actions').querySelectorAll('button').forEach(
      x => x.classList.toggle('chosen', x === b));
  targets();
  form();
}
function form() {
  const area = el('inputs');
  area.replaceChildren();
  fields = [];
  if (!action || !tool) return;
  const config = action.id === 'apply-config';
  const schema = config ? tool.schema : action.inputSchema;
  const required = schema?.required || [];
  for (const [key, s] of Object.entries(schema?.properties || {})) {
    const wrap = make('div');
    wrap.className = 'field';
    const label = make('label'), toggle = make('input');
    toggle.type = 'checkbox';
    toggle.checked = !config && required.includes(key);
    toggle.disabled = !config && required.includes(key);
    label.append(toggle, document.createTextNode(s.title || key));
    wrap.append(label);
    if (s.description) wrap.append(make('small', s.description));
    let input;
    if (s.enum || s.type === 'boolean') {
      input = make('select');
      for (const v of s.enum || [true, false]) {
        const option = make('option', display(v));
        option.value = JSON.stringify(v);
        input.append(option);
      }
    } else if (['object', 'array'].includes(s.type)) {
      input = make('textarea');
    } else {
      input = make('input');
      input.type = ['integer', 'number'].includes(s.type) ? 'number' : 'text';
    }
    input.setAttribute('aria-label', s.title || key);
    let value = config ? undefined : s.default;
    if (config && chosen.size) {
      const all =
          [...chosen].map(n => observation(n, tool.manifest.id)?.config?.[key]);
      const same = all.every(x => JSON.stringify(x) === JSON.stringify(all[0]));
      if (same) value = all[0];
      wrap.append(make(
          'small', same ? '当前值 · ' + display(value) : '当前值 · 多个值'));
    }
    if (value !== undefined)
      input.value =
          (s.enum || s.type === 'boolean' || typeof value === 'object') ?
          JSON.stringify(value) :
          String(value);
    input.oninput = () => {
      toggle.checked = true;
    };
    wrap.append(input);
    area.append(wrap);
    fields.push({key, s, toggle, input});
  }
}
function values() {
  const result = {};
  for (const {key, s, toggle, input} of fields) {
    if (!toggle.checked) continue;
    let v = input.value;
    if (s.enum || ['boolean', 'object', 'array'].includes(s.type))
      v = JSON.parse(v);
    else if (['integer', 'number'].includes(s.type)) {
      if (v === '' || !Number.isFinite(Number(v)))
        throw Error(key + ' 需要数字');
      v = Number(v);
    }
    result[key] = v;
  }
  return result;
}
async function preview() {
  if (!tool || !action || !chosen.size)
    throw Error('请先选择工具、操作和目标机器');
  const v = values();
  const cfg = action.id === 'apply-config';
  const plan = await api('preview', {
    bundle: tool.digest,
    action: action.id,
    nodes: [...chosen],
    params: cfg ? {} : v,
    values: cfg ? v : {}
  });
  const root = modal('确认发布 · ' + action.title);
  const tbl = make('table');
  table(
      tbl, ['目标机器', '目标版本', '变更或参数'],
      plan.tasks.map(
          t =>
              [t.node, t.targetVersion,
               cfg ? Object.entries(t.values)
                         .map(
                             ([k, x]) => k + '：' + display(t.expected?.[k]) +
                                 ' → ' + display(x))
                         .join('\n') :
                     JSON.stringify(t.params || {})]));
  root.append(tbl);
  const send = button('发布到 ' + plan.tasks.length + ' 台机器', async () => {
    send.disabled = true;
    try {
      await api('publish', {id: plan.id});
      el('dialog').close();
      location.hash = 'history';
      await refresh();
    } finally {
      send.disabled = false;
    }
  });
  send.className = 'primary';
  root.append(send);
}
function register() {
  const root = modal('注册工具发布包');
  const file = make('input');
  file.type = 'file';
  file.accept = '.json';
  root.append(
      make('label', '已签名的发布包'), file, button('注册文件', async () => {
        if (!file.files[0]) throw Error('请选择发布包');
        const b = JSON.parse(await file.files[0].text());
        await api('register', b);
        el('dialog').close();
        await refresh(true);
      }));
  const inputs =
      ['GitHub owner/repository', '完整提交 ID', '发布包相对路径'].map(t => {
        const l = make('label', t), i = make('input');
        l.append(i);
        root.append(l);
        return i;
      });
  root.append(button('从 GitHub 导入', async () => {
    await api('import', {
      repository: inputs[0].value,
      commit: inputs[1].value,
      path: inputs[2].value
    });
    el('dialog').close();
    await refresh(true);
  }));
}
function invite() {
  const root = modal('注册节点');
  const n = make('input'), g = make('input');
  n.placeholder = '节点名称';
  g.placeholder = '分组（可选）';
  root.append(n, g, button('生成一次性注册码', async () => {
                const r = await api('invite', {name: n.value, group: g.value});
                root.replaceChildren(
                    make('p', '将注册码交给这台机器的 Agent。'),
                    make('pre', r.invite));
                await refresh();
              }));
}
el('invite').onclick = invite;
el('register').onclick = register;
el('refresh').onclick = () => tryRun(() => refresh());
el('close').onclick = () => el('dialog').close();
el('preview').onclick = () => tryRun(preview);
el('tool').onchange = chooseTool;
el('filter').oninput = targets;
el('all').onclick = () => {
  state.nodes.filter(n => matches(n) && eligible(n))
      .forEach(n => chosen.add(n.name));
  targets();
  form();
};
el('none').onclick = () => {
  chosen.clear();
  targets();
  form();
};
el('sync').onclick = () => tryRun(async () => {
  const rows = await api('sync', {});
  const root = modal('仓库同步结果');
  if (!rows.length)
    root.append(make('p', '从 GitHub 导入发布包后，该来源将定期同步。'));
  for (const r of rows)
    root.append(make('p', r.repository + ' · ' + r.path + ' · ' + r.message));
  await refresh();
});
addEventListener('hashchange', showTab);
showTab();
tryRun(() => refresh(true));
setInterval(() => {
  if (!document.hidden && !el('dialog').open) tryRun(() => refresh());
}, 8000);
