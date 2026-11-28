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
let inventoryIndex = new Map();
let previewGeneration = 0, historyRequest = 0, historyEnd = 0, refreshRequest = 0;
let historyCursors = [0], historyMore = false, historyNavigating = false;
function table(target, head, rows) {
  target.replaceChildren();
  const header = target.createTHead().insertRow();
  head.forEach(h => {
    const cell = make('th', h);
    cell.scope = 'col';
    header.append(cell);
  });
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
  return inventoryIndex.get(name + '/' + id);
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
function connectionText(n) {
  return !n.enrolled ? '未接入' : n.online ? '在线' : '离线';
}
function badge(text, tone = '') {
  const b = make('span', text);
  b.className = 'badge ' + tone;
  return b;
}
function connectionBadge(n) {
  return badge(connectionText(n), !n.enrolled ? 'wait' : n.online ? 'ok' : '');
}
const taskTones = {
  succeeded: 'ok', dispatched: 'active', progress: 'active', queued: 'wait',
  'pending-activation': 'wait', failed: 'error', conflict: 'error', unknown: 'wait'
};
function taskBadge(t) {
  const code = t.state === 'dispatched' && t.result?.status === 'progress' ? 'progress' : t.state;
  return badge(statuses[code] || code, taskTones[code]);
}
function installPhaseList(phases, active) {
  const stages = [['preflight', '环境检查'], ['install', '执行安装'], ['verify', '安装验收']];
  const phaseLabels = {running: '执行中', 'not-run': '未执行', waiting: '等待中', unreported: '未上报', conflict: '冲突'};
  const list = make('ol');
  list.className = 'install-phases';
  list.setAttribute('aria-label', '安装阶段');
  stages.forEach(([name, title], index) => {
    const receipt = phases?.[name];
    const waiting = active && !stages.slice(index + 1).some(([later]) => phases?.[later]);
    const state = receipt?.state || (waiting ? 'waiting' : 'unreported');
    const item = make('li');
    item.append(make('span', title), badge(phaseLabels[state] || statuses[state] || state,
      state === 'running' ? 'active' : taskTones[state]));
    if (receipt?.summary || receipt?.code) {
      const reason = make('small', receipt.summary);
      if (receipt.code) reason.append(make('code', receipt.code));
      item.append(reason);
    }
    list.append(item);
  });
  return list;
}
function actionTitle(t) {
  const release = state.tools.find(v => v.digest === t.bundle);
  return labels[t.action] || release?.manifest.actions?.find(a => a.id === t.action)?.title || t.action;
}
function dateText(seconds) {
  return new Date(seconds * 1000).toLocaleString('zh-CN', {hour12: false});
}
function selectOptions(select, entries, preferred = select.value) {
  // Preserve open native menus and focus across background polling.
  const same = select.length === entries.length && entries.every(([value, label], i) =>
    select.options[i].value === value && select.options[i].textContent === label);
  if (!same) select.replaceChildren(...entries.map(([value, label]) => new Option(label, value)));
  select.value = entries.some(([value]) => value === preferred) ? preferred : entries[0]?.[0] || '';
  select.disabled = !entries.length;
}
function stack(first, second) {
  const box = make('div');
  box.className = 'stack';
  box.append(first instanceof Node ? first : make('span', first));
  if (second) box.append(make('small', second));
  return box;
}
function groupText(n) {
  return n.groups.join(' / ') || '未分组';
}
function hostMatches(n, query) {
  const q = query.trim().toLowerCase();
  return [n.name, ...n.groups].some(value => value.toLowerCase().includes(q));
}
function groupChoice(name, checked, change) {
  const label = make('label'), input = make('input');
  input.type = 'checkbox';
  input.value = name;
  input.checked = checked;
  input.onchange = () => change(input.checked);
  label.append(input, make('span', name || '未分组'));
  return label;
}
function groupFilter(id, change) {
  const picker = el(id), summary = picker.querySelector('summary'), menu = picker.querySelector('.group-menu');
  picker.hidden = true;
  const selected = new Set();
  let options = [];
  function caption() {
    summary.textContent = selected.size ? '分组 · ' + selected.size : '分组';
    summary.title = [...selected].map(s => s || '未分组').join(' / ');
  }
  function render() {
    picker.hidden = !selected.size && !options.some(Boolean);
    menu.replaceChildren(make('small', '匹配任一分组'));
    const names = new Set([...options, ...selected]);
    for (const name of names) {
      menu.append(groupChoice(name, selected.has(name), checked => {
        if (checked) selected.add(name); else selected.delete(name);
        caption();
        change();
      }));
    }
    caption();
  }
  return {
    update(names) {
      if (options.length === names.length && names.every((name, i) => name === options[i])) return;
      options = names;
      render();
    },
    matches(n) {
      return !selected.size || (n.groups.length ? n.groups.some(group => selected.has(group)) : selected.has(''));
    },
    clear() {
      selected.clear();
      picker.open = false;
      render();
    }
  };
}

function showTab() {
  let tab = location.hash.slice(1);
  const tabs = ['fleet', 'hosts', 'tools', 'workflows', 'history'];
  if (!tabs.includes(tab)) tab = 'fleet';
  for (const id of tabs) el(id).hidden = id !== tab;
  document.querySelectorAll('nav a').forEach(a => {
    if (a.hash === '#' + tab)
      a.setAttribute('aria-current', 'page');
    else
      a.removeAttribute('aria-current');
  });
  document.body.classList.toggle('canvas-view', tab === 'fleet');
  el('page-name').textContent = document.querySelector('nav a[aria-current=page]').textContent;
  if (tab === 'fleet' && state) tryRun(updateTopology);
  if (tab === 'workflows' && state) tryRun(() => workflowPage.refresh());
}
function updateTopology() {
  topology.update(state);
  return fleetViews.update(state);
}
async function showAgentVersions(name) {
  const root = modal(name + ' · Agent 版本记录');
  const loading = make('p', '正在读取版本记录');
  root.append(loading);
  const entries = await api('agent-versions?node=' + encodeURIComponent(name));
  if (!loading.isConnected || !el('dialog').open) return;
  if (!entries.length) { loading.textContent = '该主机尚未上报 Agent 版本'; return; }
  const tbl = make('table'), scroll = make('div');
  table(tbl, ['运行版本','平台','首次观察到该变化'], entries.map(e => [e.version,e.platform,dateText(e.seen)]));
  scroll.className = 'scroll'; scroll.append(tbl); loading.replaceWith(scroll);
}
function fleetTable() {
  if (!state) return;
  const ids = [...new Set(['pto-agent', ...state.tools.map(t => t.manifest.id)])];
  const query = el('fleet-filter').value.trim().toLowerCase();
  const nodes = state.nodes.filter(n => hostGroups.matches(n) && hostMatches(n, query));
  const rows = nodes.map(n => {
    const link = button(n.name, () => topology.showHost(n.name));
    link.className = 'host-link ident';
    link.title = '在拓扑中查看 ' + n.name;
    const host = stack(link, groupText(n));
    if (state.role === 'admin') {
      const edit = button('编辑分组', () => editGroups(n));
      edit.className = 'group-edit';
      edit.setAttribute('aria-label', '编辑 ' + n.name + ' 的分组');
      host.append(edit);
      if (n.enrolled) {
        const run = button('运行工作流', () => workflowPage.forNode(n.name));
        run.className = 'group-edit'; run.setAttribute('aria-label', '在 ' + n.name + ' 上运行工作流');
        host.append(run);
      }
    }
    return [host, connectionBadge(n), ...ids.map(id => {
      const o = observation(n.name, id);
      if (id === 'pto-agent') {
        if (!o?.runningVersion) return badge('未上报');
        const history = button(o.runningVersion, () => showAgentVersions(n.name));
        history.className = 'group-edit';
        history.setAttribute('aria-label', n.name + ' 的 Agent 版本记录');
        return stack(history, o.platform);
      }
      if (!o) return badge('等待盘点', 'wait');
      if (o.presence !== 'present') return badge(presence[o.presence] || '未知');
      const versions = '安装 ' + (o.installedVersion || '未知') + ' / 运行 ' + (o.runningVersion || '未知');
      const cell = stack(versions, o.writtenRevision
          ? '配置写入 ' + o.writtenRevision + ' / 生效 ' + (o.effectiveRevision || '待核实') : '');
      if (o.seen) cell.append(make('small', '盘点 ' + dateText(o.seen)));
      return cell;
    })];
  });
  table(el('matrix'), ['主机 / 分组', '连接', ...ids], rows);
  el('fleet-empty').hidden = rows.length > 0;
  el('fleet-empty').textContent = state.nodes.length ? '没有匹配的主机' : '尚无已登记的主机';
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
  const target = el('history-table');
  const open = new Set([...target.querySelectorAll('details[open]')].map(d => d.dataset.id));
  table(target, ['主机 / 工具', '操作', '状态', '结果', '操作人 / 时间'], tasks.map(t => {
    const details = make('details');
    details.dataset.id = t.taskId;
    details.open = open.has(t.taskId);
    details.append(make('summary', t.result?.summary || '查看详情'));
    if (t.action === 'install') details.append(installPhaseList(t.result?.phases, ['queued', 'dispatched'].includes(t.state)));
    details.append(make('pre', JSON.stringify(t, null, 2)));
    return [stack(t.node, t.tool), actionTitle(t), taskBadge(t), details,
            stack(t.actor, dateText(t.created))];
  }));
  el('history-count').textContent = '本页 ' + tasks.length + ' 条';
  el('history-empty').hidden = tasks.length > 0;
}
async function refresh() {
  const request = ++refreshRequest;
  const response = await api('state');
  if (request !== refreshRequest) return;
  state = response;
  // Older centers report the original single group.
  for (const n of state.nodes) n.groups ??= n.group ? [n.group] : [];
  const groups = [...new Set(state.nodes.flatMap(n => n.groups.length ? n.groups : ['']))].sort();
  for (const filter of [mapGroups, hostGroups, targetGroups]) filter.update(groups);
  inventoryIndex = new Map(state.inventory.map(o => [o.node + '/' + o.tool, o]));
  el('role').hidden = false;
  el('role').textContent = state.role === 'admin' ? '管理员' : '只读用户';
  el('updated').textContent = '刷新于 ' + new Date().toLocaleTimeString('zh-CN', {hour12: false});
  fleetTable();
  document.querySelectorAll('.admin').forEach(
      e => e.hidden = state.role !== 'admin');
  renderCenterUpdate();
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
  if (!el('fleet').hidden) await updateTopology();
  if (!el('workflows').hidden) await workflowPage.refresh();
}
let centerRequest, centerSending = false;
function renderCenterUpdate() {
  const info = state.centerUpdate, panel = el('center-update');
  panel.hidden = !info;
  if (!info) return;
  el('center-running').textContent = info.running + ' · ' + info.platform;
  const available = info.releases.filter(tool => tool.platforms.includes(info.platform));
  const select = el('center-target'), previous = select.value;
  select.replaceChildren(...available.map(tool => new Option(tool.version, tool.digest)));
  if (available.some(tool => tool.digest === previous)) select.value = previous;
  const last = info.last;
  el('center-result').textContent = last ? last.expected + ' → ' + last.version + ' · ' + last.summary :
      !info.managed ? '中心机执行器未启用' : !available.length ? '等待注册本机架构的中心签名包' : '';
  const updateButtons = () => {
    const target = available.find(tool => tool.digest === select.value);
    const disabled = !target || target.version === info.running || !info.managed || centerSending ||
        ['queued', 'running'].includes(last?.state);
    el('center-upgrade').disabled = el('center-rollback').disabled = disabled;
  };
  select.onchange = updateButtons;
  updateButtons();
}
async function switchCenter(action) {
  const bundle = el('center-target').value, expected = state.centerUpdate.running;
  if (!centerRequest || centerRequest.action !== action || centerRequest.bundle !== bundle || centerRequest.expected !== expected) {
    const bytes = crypto.getRandomValues(new Uint8Array(32));
    const id = Array.from(bytes, byte => byte.toString(16).padStart(2, '0')).join('');
    centerRequest = {id, action, bundle, expected};
  }
  centerSending = true;
  renderCenterUpdate();
  try {
    const result = await api('center-update', centerRequest);
    centerRequest = undefined;
    el('center-result').textContent = result.summary;
    await refresh();
  } finally {
    centerSending = false;
    renderCenterUpdate();
  }
}
function chooseTool() {
  ++previewGeneration;
  tool = state.tools.find(t => t.digest === el('tool').value);
  el('tool-empty').hidden = !!tool;
  el('tool').disabled = !tool;
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
    b.setAttribute('aria-pressed', 'false');
    el('actions').append(b);
    if (!action) selectAction(a, b);
  }
  targets();
}
function matches(n) {
  return targetGroups.matches(n) && hostMatches(n, el('filter').value);
}
function eligible(n) {
  if (!n.enrolled) return false;
  if (tool?.manifest.id === 'pto-agent' && agentTargetHint(n)) return false;
  if (!action || action.id === 'probe') return true;
  const o = observation(n.name, tool.manifest.id);
  if (action.id === 'install') return o?.presence === 'absent';
  return o?.presence === 'present' &&
      (!o.capabilities || o.capabilities.includes(action.id));
}
function agentTargetHint(n) {
  const o = observation(n.name, 'pto-agent');
  if (!o?.platform) return '尚未上报 Agent 版本';
  if (!action || action.id === 'probe') return '';
  if (!o.capabilities?.includes(action.id)) return '此 Agent 未启用托管升级';
  return tool.platforms?.includes(o.platform) ? '' : '发布包缺少 ' + o.platform + ' 程序';
}
function selectionSummary() {
  const size = chosen.size;
  const hidden = state.nodes.filter(n => chosen.has(n.name) && !matches(n)).length;
  const missingField = action?.id === 'apply-config' && !fields.some(f => f.toggle.checked);
  const count = '已选 ' + size + ' 台';
  if (el('selected-count').textContent !== count) el('selected-count').textContent = count;
  const hint = size ? count + (hidden ? '，其中 ' + hidden + ' 台在筛选外' : '') : '请选择目标机器';
  el('selection-summary').textContent = hint + (size && missingField ? ' · 请选择配置字段' : '');
  el('preview').disabled = !action || !size || missingField;
  el('none').disabled = !size;
  el('all').disabled = !state.nodes.some(n => matches(n) && eligible(n) && !chosen.has(n.name));
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
  const single = action?.supportsBatch === false;
  const list = el('targets'), focus = document.activeElement.dataset.target;
  list.replaceChildren();
  const filtered = state.nodes.filter(matches);
  for (const n of filtered) {
    const label = make('label'), cb = make('input');
    cb.type = single ? 'radio' : 'checkbox';
    cb.name = 'target-node';
    cb.dataset.target = n.name;
    cb.checked = chosen.has(n.name);
    cb.disabled = !allowed.has(n.name);
    cb.onchange = () => {
      ++previewGeneration;
      if (single) chosen.clear();
      if (cb.checked) chosen.add(n.name);
      else chosen.delete(n.name);
      updateBaselines();
      selectionSummary();
    };
    const o = observation(n.name, tool.manifest.id);
    const why = cb.disabled && n.enrolled
        ? (tool.manifest.id === 'pto-agent' ? agentTargetHint(n) : !o ? '等待盘点' : action?.id === 'install' && o.presence === 'present' ? '已安装' :
            o.presence === 'present' ? '不支持此操作' : presence[o.presence]) : '';
    label.append(cb, stack(n.name, [groupText(n), why].filter(Boolean).join(' · ')), connectionBadge(n));
    list.append(label);
    if (focus === n.name) cb.focus({preventScroll: true});
  }
  if (!filtered.length) {
    const empty = make('p', state.nodes.length ? '没有匹配的机器' : '尚无已登记的机器');
    empty.className = 'empty';
    list.append(empty);
  }
  el('all').hidden = single;
  updateBaselines();
  selectionSummary();
}
function selectAction(a, b) {
  if (action?.id === a.id) return;
  ++previewGeneration;
  action = a;
  if (a.supportsBatch === false && chosen.size > 1) chosen.clear();
  fields = [];
  el('actions').querySelectorAll('button').forEach(x => {
    x.classList.toggle('chosen', x === b);
    x.setAttribute('aria-pressed', String(x === b));
  });
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
function schemaField(key, s) {
  const wrap = make('div'), label = make('label'), toggle = make('input');
  wrap.className = 'field';
  toggle.type = 'checkbox';
  label.append(toggle, document.createTextNode(s.title || key));
  wrap.append(label);
  if (s.description) wrap.append(make('small', s.description));
  let input;
  if (s.enum || s.type === 'boolean') {
    input = make('select');
    for (const v of s.enum || [true, false]) input.append(new Option(display(v), JSON.stringify(v)));
  } else if (jsonField(s)) {
    input = make('textarea');
  } else {
    input = make('input');
    input.type = ['integer', 'number'].includes(s.type) ? 'number' : 'text';
    if (s.type === 'number') input.step = 'any';
  }
  input.setAttribute('aria-label', s.title || key);
  wrap.append(input);
  return {key, s, wrap, toggle, input};
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
    const field = schemaField(key, s);
    const {wrap, toggle, input} = field;
    toggle.checked = !config && required.includes(key);
    toggle.disabled = !config && required.includes(key);
    field.baseline = make('small');
    if (config) input.before(field.baseline);
    else if (s.default !== undefined) setValue(input, s, s.default);
    field.edited = false;
    toggle.onchange = () => {
      field.edited = true;
      ++previewGeneration;
      selectionSummary();
    };
    input.oninput = () => {
      field.edited = true;
      ++previewGeneration;
      toggle.checked = true;
      selectionSummary();
    };
    area.append(wrap);
    fields.push(field);
  }
  updateBaselines();
  selectionSummary();
}
function values(source = fields) {
  const result = Object.create(null);
  for (const {key, s, toggle, input} of source) {
    if (!toggle.checked) continue;
    let v = input.value;
    if (jsonField(s)) {
      try { v = JSON.parse(v); } catch { throw Error((s.title || key) + ' 需要有效的 JSON 值'); }
    } else if (['integer', 'number'].includes(s.type)) {
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
  const context = make('p', tool.manifest.id + ' · ' + tool.version + ' · ' + plan.tasks.length + ' 台目标机器');
  context.className = 'preview-context';
  const scroll = make('div');
  scroll.className = 'scroll';
  const tbl = make('table');
  const rows = plan.tasks.map(t => {
    if (!cfg) return [t.node, t.targetVersion, JSON.stringify(t.params || {})];
    const changes = make('div');
    changes.className = 'changes';
    for (const [key, value] of Object.entries(t.values)) {
      const title = fields.find(f => f.key === key)?.s.title || key;
      const change = stack(display(t.expected?.[key]) + ' → ' + display(value), title);
      change.title = key;
      changes.append(change);
    }
    return [t.node, t.targetVersion, changes];
  });
  table(tbl, ['目标机器', '目标版本', cfg ? '字段变更' : '操作参数'], rows);
  scroll.append(tbl);
  root.append(context, scroll);
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
  const footer = make('div');
  footer.className = 'dialog-actions';
  footer.append(button('返回修改', () => el('dialog').close()), send);
  root.append(footer);
}
function register() {
  const root = modal('注册工具发布包');
  const file = make('input');
  file.type = 'file';
  file.accept = '.json';
  const fileLabel = make('label', '已签名的发布包');
  fileLabel.append(file);
  root.append(
      fileLabel, button('注册文件', async () => {
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
function groupEditor(root, current) {
  const selected = new Set(current);
  const names = new Set(state.nodes.flatMap(n => n.groups).concat(current).sort());
  const field = make('fieldset'), choices = make('div'), entry = make('div'), input = make('input');
  field.className = 'group-editor';
  choices.className = 'group-choices';
  entry.className = 'group-entry';
  input.placeholder = '新分组名称';
  input.setAttribute('aria-label', input.placeholder);
  function draw() {
    choices.replaceChildren(...[...names].map(name => groupChoice(name, selected.has(name), checked => {
      if (checked) selected.add(name); else selected.delete(name);
    })));
  }
  function add() {
    const name = input.value.trim();
    if (!name) return;
    names.add(name);
    selected.add(name);
    input.value = '';
    draw();
  }
  input.onkeydown = e => {
    if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); add(); }
  };
  entry.append(input, button('添加分组', add));
  field.append(make('legend', '所属分组（可多选）'), choices, entry);
  root.append(field);
  draw();
  return () => { add(); return [...selected]; };
}
function editGroups(node) {
  const root = modal('编辑分组 · ' + node.name);
  const groups = groupEditor(root, node.groups);
  root.append(button('保存分组', async () => {
    await api('node-groups', {name: node.name, expectedGroups: node.groups, groups: groups()});
    el('dialog').close();
    await refresh();
  }));
}
function invite() {
  const root = modal('注册节点'), name = make('input'), label = make('label', '节点名称');
  label.append(name);
  root.append(label);
  const groups = groupEditor(root, []);
  root.append(button('生成一次性注册码', async () => {
    const r = await api('invite', {name: name.value, groups: groups()});
    root.replaceChildren(make('p', '将注册码交给这台机器的 Agent。'), make('pre', r.invite));
    await refresh();
  }));
}
const narrowNav = matchMedia('(max-width: 760px)');
function setSidebar(open) {
  document.body.classList.toggle('nav-closed', !open);
  el('sidebar').inert = !open;
  const toggle = el('nav-toggle');
  toggle.setAttribute('aria-expanded', String(open));
  toggle.title = open ? '收起侧栏' : '展开侧栏';
  toggle.setAttribute('aria-label', toggle.title);
}
setSidebar(!narrowNav.matches);
narrowNav.addEventListener('change', () => setSidebar(!narrowNav.matches));
el('nav-toggle').onclick = () => setSidebar(document.body.classList.contains('nav-closed'));
el('sidebar').addEventListener('click', event => {
  if (narrowNav.matches && event.target.closest('a')) setSidebar(false);
});
document.addEventListener('keydown', event => {
  if (event.key === 'Escape' && narrowNav.matches && !document.body.classList.contains('nav-closed')) {
    setSidebar(false);
    el('nav-toggle').focus();
  }
});
const mapGroups = groupFilter('map-groups', () => topology.filterChanged());
const hostGroups = groupFilter('host-groups', fleetTable);
const targetGroups = groupFilter('target-groups', targets);
const topology = createTopology();
const fleetViews = createFleetViews(() => topology.redraw());
const workflowPage = createWorkflowPage();
document.addEventListener('click', event => {
  for (const picker of document.querySelectorAll('.group-picker[open]')) {
    if (!picker.contains(event.target)) picker.open = false;
  }
});
document.addEventListener('keydown', event => {
  const picker = event.target.closest('.group-picker[open]');
  if (event.key === 'Escape' && picker) {
    picker.open = false;
    picker.querySelector('summary').focus();
  }
});
document.querySelector('.skip').onclick = e => { e.preventDefault(); el('main').focus(); };
el('fleet-filter').oninput = fleetTable;
el('host-invite').onclick = invite;
el('invite').onclick = invite;
el('register').onclick = register;
el('platform-sync').onclick = () => tryRun(async () => {
  el('platform-sync').disabled = true;
  try {
    const tools = await api('platform-sync', {});
    notice('已登记 ' + tools.map(t => t.manifest.id + ' ' + t.version).join('、'));
    await refresh();
  } finally {
    el('platform-sync').disabled = false;
  }
});
el('center-upgrade').onclick = () => tryRun(() => switchCenter('update'));
el('center-rollback').onclick = () => tryRun(() => switchCenter('rollback'));
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
