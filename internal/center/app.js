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
let previewGeneration = 0, historyRequest = 0, historyEnd = 0, refreshRequest = 0;
let historyCursors = [0], historyMore = false, historyNavigating = false;
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
  ++previewGeneration;
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
}
async function renderHistory(cursors = historyCursors) {
  if (historyNavigating) return;
  const request = ++historyRequest;
  historyNavigating = cursors !== historyCursors;
  if (historyNavigating) {
    el('older').disabled = true;
    el('newer').disabled = true;
  }
  try {
    const cursor = cursors.at(-1);
    const page = cursor ? await api('history?before=' + cursor) : state;
    if (request !== historyRequest) return;
    drawHistory(page.tasks);
    historyCursors = cursors;
    historyEnd = page.tasks.at(-1)?.seq || 0;
    historyMore = page.moreTasks;
  } catch (err) {
    if (request === historyRequest) throw err;
  } finally {
    if (request === historyRequest) {
      historyNavigating = false;
      el('older').disabled = !historyMore;
      el('newer').disabled = historyCursors.length === 1;
    }
  }
}
function drawHistory(tasks) {
  table(
      el('history-table'),
      ['节点 / 工具', '操作', '状态', '结果', '操作人 / 时间'],
      tasks.map(t => {
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
async function refresh() {
  const request = ++refreshRequest;
  const response = await api('state');
  if (request !== refreshRequest) return;
  state = response;
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
  if (tool?.digest !== el('tool').value)
    chooseTool();
  else
    targets();
  await renderHistory();
}
function chooseTool() {
  ++previewGeneration;
  tool = state.tools.find(t => t.digest === el('tool').value);
  el('tool-empty').hidden = !!tool;
  el('operations').hidden = !tool;
  chosen.clear();
  action = null;
  fields = [];
  if (!tool) return;
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
  const allowed = new Set(state.nodes.filter(eligible).map(n => n.name));
  for (const name of chosen) {
    if (!allowed.has(name)) {
      chosen.delete(name);
      ++previewGeneration;
    }
  }
  el('targets').replaceChildren();
  for (const n of state.nodes.filter(matches)) {
    const label = make('label'), cb = make('input');
    cb.type = 'checkbox';
    cb.checked = chosen.has(n.name);
    cb.disabled = !eligible(n);
    cb.onchange = () => {
      ++previewGeneration;
      if (cb.checked)
        chosen.add(n.name);
      else
        chosen.delete(n.name);
      updateBaselines();
    };
    label.append(
        cb, document.createTextNode(n.name + (n.online ? '' : ' · 离线')));
    el('targets').append(label);
  }
  updateBaselines();
}
function selectAction(a, b) {
  if (action?.id === a.id) return;
  ++previewGeneration;
  action = a;
  fields = [];
  el('actions').querySelectorAll('button').forEach(
      x => x.classList.toggle('chosen', x === b));
  targets();
  form();
}
// Resolve JSON Pointer fragments (including escaped keys) and local anchors.
// Validation of the full schema remains on the center and agent.
function fieldSchema(root, source, seen = new Set()) {
  if (!source || typeof source !== 'object') return {};
  if (!source.$ref || seen.has(source)) return source;
  seen.add(source);
  if (!source.$ref.startsWith('#')) return source;
  const fragment = decodeURIComponent(source.$ref.slice(1));
  let ref = root;
  if (fragment.startsWith('/')) {
    for (const part of fragment.slice(1).split('/')) {
      const key = part.replaceAll('~1', '/').replaceAll('~0', '~');
      if (!ref || !Object.hasOwn(ref, key)) return source;
      ref = ref[key];
    }
  } else if (fragment) {
    const pending = [root];
    ref = null;
    while (pending.length) {
      const value = pending.pop();
      if (value.$anchor === fragment || value.$dynamicAnchor === fragment) { ref = value; break; }
      for (const child of Object.values(value)) {
        if (child && typeof child === 'object') pending.push(child);
      }
    }
  }
  const target = fieldSchema(root, ref, seen);
  const merged = {...target, ...source};
  if (target.required && source.required)
    merged.required = [...new Set([...target.required, ...source.required])];
  if (target.properties && source.properties) {
    merged.properties = Object.assign(Object.create(null), target.properties);
    for (const [name, local] of Object.entries(source.properties)) {
      const inherited = target.properties[name];
      merged.properties[name] = local === true && inherited !== undefined ? inherited
          : inherited && typeof local === 'object' ? {...inherited, ...local} : local;
    }
  }
  return merged;
}
function jsonField(s) {
  return !!s.enum || !['integer', 'number', 'string'].includes(s.type);
}
function setValue(input, s, value) {
  input.value = value === undefined ? '' : jsonField(s) ? JSON.stringify(value) : String(value);
}
function updateBaselines() {
  if (action?.id !== 'apply-config') return;
  for (const f of fields) {
    const all = [...chosen].map(n => observation(n, tool.manifest.id)
        ?.configBySchema?.[tool.manifest.configSchemaVersion]?.[f.key]);
    const same = all.every(x => JSON.stringify(x) === JSON.stringify(all[0]));
    f.baseline.textContent = all.length ? '当前值 · ' + (same ? display(all[0]) : '多个值') : '';
    if (!f.edited) setValue(f.input, f.s, same ? all[0] : undefined);
  }
}
function form() {
  const area = el('inputs');
  area.replaceChildren();
  fields = [];
  if (!action || !tool) return;
  const config = action.id === 'apply-config';
  const schema = config ? tool.schema : action.inputSchema;
  const layout = fieldSchema(schema, schema);
  const required = layout.required || [];
  for (const [key, property] of Object.entries(layout.properties || {})) {
    const s = fieldSchema(schema, property);
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
    } else if (jsonField(s)) {
      input = make('textarea');
    } else {
      input = make('input');
      input.type = ['integer', 'number'].includes(s.type) ? 'number' : 'text';
    }
    input.setAttribute('aria-label', s.title || key);
    const baseline = make('small');
    if (config) wrap.append(baseline);
    else if (s.default !== undefined) setValue(input, s, s.default);
    const field = {key, s, toggle, input, baseline, edited: false};
    toggle.onchange = () => {
      field.edited = true;
      ++previewGeneration;
    };
    input.oninput = () => {
      field.edited = true;
      ++previewGeneration;
      toggle.checked = true;
    };
    wrap.append(input);
    area.append(wrap);
    fields.push(field);
  }
  updateBaselines();
}
function values() {
  const result = Object.create(null);
  for (const {key, s, toggle, input} of fields) {
    if (!toggle.checked) continue;
    let v = input.value;
    if (jsonField(s))
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
  const selectedAction = action;
  const generation = ++previewGeneration;
  const cfg = selectedAction.id === 'apply-config';
  let plan;
  try {
    plan = await api('preview', {
      bundle: tool.digest,
      action: selectedAction.id,
      nodes: [...chosen],
      params: cfg ? {} : v,
      values: cfg ? v : {}
    });
  } catch (err) {
    if (generation === previewGeneration) throw err;
    return;
  }
  if (generation !== previewGeneration) return;
  const root = modal('确认发布 · ' + selectedAction.title);
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
      ++historyRequest;
      historyNavigating = false;
      historyCursors = [0];
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
        await refresh();
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
    await refresh();
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
  ++previewGeneration;
  state.nodes.filter(n => matches(n) && eligible(n))
      .forEach(n => chosen.add(n.name));
  targets();
};
el('none').onclick = () => {
  ++previewGeneration;
  chosen.clear();
  targets();
};
el('older').onclick = () => tryRun(() => {
  if (!historyEnd || el('older').disabled) return;
  return renderHistory([...historyCursors, historyEnd]);
});
el('newer').onclick = () => tryRun(() => {
  if (historyCursors.length > 1 && !el('newer').disabled)
    return renderHistory(historyCursors.slice(0, -1));
});
el('sync').onclick = () => tryRun(async () => {
  const rows = await api('sync', {});
  const root = modal('仓库同步结果');
  if (!rows.length)
    root.append(make('p', '从 GitHub 导入发布包后，该来源将定期同步。'));
  for (const r of rows)
    root.append(make('p', r.repository + ' · ' + r.path + ' · ' + r.message));
  await refresh();
});
addEventListener('hashchange', () => { ++previewGeneration; showTab(); });
showTab();
tryRun(refresh);
setInterval(() => {
  if (!document.hidden && !el('dialog').open) tryRun(() => refresh());
}, 8000);
