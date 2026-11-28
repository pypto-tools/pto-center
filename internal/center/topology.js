'use strict';

const centerKey = '@center';
const modeKey = 'pto-center.topology.expanded.v1';

function serviceLabel(unit) {
  if (unit.active === 'active' && unit.sub === 'running') return '运行中';
  return ({active: '已激活', inactive: '已停止', failed: '失败', activating: '启动中',
    deactivating: '停止中', reloading: '重载中', refreshing: '刷新中', maintenance: '维护中'})[unit.active] || unit.active;
}
function serviceRow(unit) {
  const row = make('li');
  row.className = 'service-row';
  const name = make('span', unit.unit), status = make('span', serviceLabel(unit));
  name.className = 'service-name';
  name.title = unit.unit;
  const tone = {active: 'running', failed: 'failed', activating: 'changing', deactivating: 'changing', reloading: 'changing'};
  status.className = 'service-state ' + (tone[unit.active] || '');
  status.title = unit.active + ' / ' + unit.sub;
  row.append(name, status);
  return row;
}
function sampleLabel(node) {
  if (!node.statusAt) return '尚无采样';
  const age = Date.now() / 1000 - node.statusAt;
  const date = new Date(node.statusAt * 1000);
  const time = age < 86400 ? date.toLocaleTimeString('zh-CN', {hour: '2-digit', minute: '2-digit', hour12: false})
      : date.toLocaleDateString('zh-CN');
  const kind = !node.online ? '离线快照' : age > 90 ? '采样已过期' : '采样';
  return kind + ' ' + time;
}
function showServices(node) {
  const root = modal(node.name + ' · 系统服务');
  const all = node.status.services;
  const subtitle = make('p', sampleLabel(node) + ' · systemd' +
    (node.status.servicesTruncated ? ' · 仅显示前 ' + all.length + ' 项' : ' · ' + all.length + ' 项'));
  subtitle.className = 'subtle';
  const label = make('label', '查找服务');
  const search = make('input');
  search.type = 'search';
  search.id = 'service-search';
  label.append(search);
  const list = make('ul'), empty = make('p', '没有匹配的服务');
  list.className = 'service-list dialog-services';
  function filter() {
    const q = search.value.trim().toLowerCase();
    list.replaceChildren(...all.filter(s => s.unit.toLowerCase().includes(q)).map(serviceRow));
    empty.hidden = !!list.children.length;
  }
  search.oninput = filter;
  filter();
  root.append(subtitle, label, list, empty);
  search.focus();
}

function createTopology() {
  const host = el('map'), vertices = el('vertices'), canvas = el('connections');
  const pen = canvas.getContext('2d');
  let expanded = new Set();
  try {
    const saved = JSON.parse(localStorage.getItem(modeKey));
    if (Array.isArray(saved)) expanded = new Set(saved.filter(k => typeof k === 'string'));
  } catch {}
  let state, statusFilter = 'all', graph = [], world = {w: 1, h: 1};
  let scale = 1, tx = 0, ty = 0, followFit = true, compactLayout = false;
  let lastState = '', focusOnShow = null, lastWidth = 0, lastHeight = 0;
  const pointers = new Map();
  const clamp = (v, min, max) => Math.max(min, Math.min(max, v));
  const statusOf = n => !n.enrolled ? 'pending' : n.online ? 'online' : 'offline';

  function saveModes() {
    try { localStorage.setItem(modeKey, JSON.stringify([...expanded])); } catch {}
  }
  function detail(node, core, lens) {
    const body = make('div');
    body.className = 'machine-detail';
    for (const [label, value] of lens?.facts || []) {
      const row = make('div');
      row.className = 'detail-row lens-fact';
      row.append(make('span', label), make('strong', value));
      body.append(row);
    }
    if (lens?.receipt) {
      const row = make('div'), receipt = lens.receipt;
      row.className = 'detail-row';
      const open = button(receipt.summary ? '查看回执' : '查看任务', receipt.open);
      open.className = 'service-open'; open.dataset.control = 'receipt';
      row.append(make('span', '更新任务'), open);
      body.append(row);
      if (receipt.summary) {
        const summary = make('p', receipt.summary);
        summary.className = 'detail-row receipt-summary';
        body.append(summary);
      }
    }
    if (!core) {
      const groups = make('div');
      groups.className = 'detail-row membership';
      groups.append(make('span', '所属分组'), make('strong', groupText(node)));
      body.append(groups);
      const agent = make('div'), inv = observation(node.name, 'pto-agent');
      agent.className = 'detail-row';
      agent.append(make('span', 'Agent 运行版本'), make('strong', inv?.runningVersion || '未上报'));
      body.append(agent);
    }
    const logins = make('div');
    logins.className = 'detail-row';
    const count = node.status?.loggedInUsers;
    logins.append(make('span', '登录用户'), make('strong', count == null ? '未知' : count + ' 人'));
    const units = node.status?.services;
    const title = make('div');
    title.className = 'detail-row';
    title.append(make('span', '系统服务'));
    body.append(logins, title);
    if (units?.length) {
      const all = button('查看 ' + units.length + (node.status.servicesTruncated ? '+' : '') + ' 项', () => showServices(node));
      all.className = 'service-open';
      all.dataset.control = 'services';
      title.append(all);
      const rows = make('ul');
      rows.className = 'service-list';
      rows.append(...units.slice(0, 3).map(serviceRow));
      body.append(rows);
    } else {
      title.append(make('strong', units ? '0 项' : '未知'));
    }
    const footer = make('div'), sampled = make('span', sampleLabel(node)), link = make('a', core ? '主机清单' : '该机工具');
    footer.className = 'detail-foot';
    sampled.title = node.statusAt ? dateText(node.statusAt) : '尚未收到主机状态';
    link.href = '#hosts';
    link.dataset.control = 'inventory';
    link.onclick = () => { hostGroups.clear(); el('fleet-filter').value = core ? '' : node.name; fleetTable(); };
    footer.append(sampled, link);
    body.append(footer);
    return body;
  }

  function card(node, key) {
    const core = key === centerKey, open = expanded.has(key);
    const lens = fleetViews.nodeCard(node, core);
    const item = make('article'), header = make('button');
    item.dataset.name = core ? '' : node.name;
    item.className = 'vertex ' + (core ? 'core' : statusOf(node)) + (open ? ' expanded' : '');
    if (lens) item.dataset.tone = lens.tone;
    item.setAttribute('aria-label', node.name);
    header.type = 'button';
    header.className = 'node-toggle';
    header.dataset.control = 'mode';
    header.setAttribute('aria-expanded', String(open));
    header.setAttribute('aria-label', node.name + (open ? '，切换到简约模式' : '，切换到详细模式'));
    header.title = header.getAttribute('aria-label');
    header.onclick = () => {
      const before = item.getBoundingClientRect();
      if (expanded.has(key)) expanded.delete(key); else expanded.add(key);
      saveModes();
      render();
      if (!followFit) {
        const after = graph.find(n => n.key === key).item.getBoundingClientRect();
        tx += before.left - after.left; ty += before.top - after.top;
        draw();
      }
    };
    const icon = make('span'), glyph = make('i'), dot = make('i');
    icon.className = 'machine-icon';
    icon.setAttribute('aria-hidden', 'true');
    glyph.className = 'glyph ' + (core ? 'map-icon' : 'host-icon');
    dot.className = 'status-dot';
    icon.append(glyph, dot);
    const lines = make('span'), name = make('strong', node.name);
    name.title = node.name;
    lines.className = 'node-id';
    const connection = core ? ['中心服务', state.centerUpdate?.running].filter(Boolean).join(' · ') :
        (open ? 'Agent' : groupText(node)) + ' · ' + connectionText(node);
    const subline = make('small', lens?.text || connection);
    subline.title = subline.textContent;
    lines.append(name, subline);
    const mode = make('span', open ? '简约' : '详情');
    mode.className = 'mode-label';
    header.append(icon, lines, mode);
    item.append(header);
    if (open) item.append(detail(node, core, lens));
    vertices.append(item);
    return {key, node, item, x: 0, y: 0, w: item.offsetWidth, h: item.offsetHeight, port: header.offsetHeight / 2 + 1};
  }

  function layout() {
    const [core, ...agents] = graph;
    const gap = compactLayout ? 15 : 23;
    if (compactLayout) {
      const trunk = 24, inset = 46;
      core.x = 0;
      let y = core.h + gap * 1.5;
      for (const n of agents) {
        n.x = inset; n.y = y; n.trunk = trunk;
        y += n.h + gap;
      }
    } else {
      const across = agents.length > 7;
      const left = across ? agents.filter((_, i) => i % 2 === 0) : [];
      const right = across ? agents.filter((_, i) => i % 2 !== 0) : agents;
      const lane = clamp(host.clientWidth * .19, 105, 245);
      const columnHeight = list => list.reduce((sum, n) => sum + n.h + gap, -gap);
      const height = Math.max(core.h, columnHeight(left), columnHeight(right));
      core.x = left.length ? left.reduce((w, n) => Math.max(w, n.w), 0) + lane : 0;
      core.y = (height - core.h) / 2;
      for (const [list, side] of [[left, -1], [right, 1]]) {
        let y = (height - columnHeight(list)) / 2;
        for (const n of list) {
          n.x = side > 0 ? core.x + core.w + lane : core.x - lane - n.w;
          n.y = y; n.side = side;
          y += n.h + gap;
        }
      }
    }
    world = {w: 1, h: 1};
    for (const n of graph) {
      n.item.style.left = n.x + 'px';
      n.item.style.top = n.y + 'px';
      world.w = Math.max(world.w, n.x + n.w);
      world.h = Math.max(world.h, n.y + n.h);
    }
  }
  function draw() {
    if (!graph.length || !host.clientWidth) return;
    const w = host.clientWidth, h = host.clientHeight, ratio = window.devicePixelRatio || 1;
    if (canvas.width !== Math.round(w * ratio) || canvas.height !== Math.round(h * ratio)) {
      canvas.width = Math.round(w * ratio); canvas.height = Math.round(h * ratio);
    }
    vertices.style.transform = 'translate(' + tx + 'px,' + ty + 'px) scale(' + scale + ')';
    const pitch = Math.max(8, 16 * scale);
    host.style.backgroundSize = pitch + 'px ' + pitch + 'px';
    host.style.backgroundPosition = tx % pitch + 'px ' + ty % pitch + 'px';
    el('zoom-level').value = Math.round(scale * 100) + '%';
    el('zoom-in').disabled = scale >= 2;
    el('zoom-out').disabled = scale <= Math.min(.35, fitScale());
    if (!pen) return;
    pen.setTransform(ratio, 0, 0, ratio, 0, 0);
    pen.clearRect(0, 0, w, h);
    pen.translate(tx, ty); pen.scale(scale, scale);
    const [core, ...agents] = graph;
    for (const n of agents) {
      if (!n.node.enrolled) continue;
      const on = n.node.online;
      pen.strokeStyle = on ? '#628f80' : '#767880';
      pen.fillStyle = on ? '#81c4a7' : '#8c8e96';
      pen.lineWidth = 1.35;
      pen.setLineDash(on ? [] : [5, 5]);
      pen.beginPath();
      const endY = n.y + n.port;
      let endX;
      if (compactLayout) {
        endX = n.x;
        pen.moveTo(n.trunk, core.y + core.h);
        pen.lineTo(n.trunk, endY - 10);
        pen.quadraticCurveTo(n.trunk, endY, n.trunk + 10, endY);
        pen.lineTo(endX, endY);
      } else {
        const startX = n.side > 0 ? core.x + core.w : core.x;
        endX = n.side > 0 ? n.x : n.x + n.w;
        const startY = core.y + core.port, bend = (endX - startX) * .52;
        pen.moveTo(startX, startY);
        pen.bezierCurveTo(startX + bend, startY, endX - bend, endY, endX, endY);
      }
      pen.stroke();
      pen.beginPath(); pen.arc(endX, endY, 2.5, 0, 2 * Math.PI); pen.fill();
    }
  }
  function frame() {
    const top = el('map-view').closest('.map-top').offsetHeight + (compactLayout ? 28 : 42);
    const bottom = el('map-fit').closest('.map-bottom').offsetHeight + 30;
    const w = host.clientWidth, h = host.clientHeight;
    return {top, w, h: Math.max(60, h - top - bottom)};
  }
  function fitScale() {
    const f = frame(), margin = compactLayout ? 24 : 68;
    return Math.min(Math.max(1, f.w - margin) / world.w, f.h / world.h, 1.18);
  }
  function fit() {
    if (!graph.length || !host.clientWidth) return;
    const f = frame();
    scale = fitScale();
    tx = (f.w - world.w * scale) / 2;
    ty = f.top + Math.max(0, (f.h - world.h * scale) / 2);
    followFit = true;
    draw();
  }
  function zoom(next, x = host.clientWidth / 2, y = host.clientHeight / 2) {
    const before = scale;
    scale = clamp(next, Math.min(.35, fitScale()), 2);
    tx = x - (x - tx) * scale / before;
    ty = y - (y - ty) * scale / before;
    followFit = false;
    draw();
  }
  function render() {
    if (!state || !host.clientWidth) return;
    lastWidth = host.clientWidth; lastHeight = host.clientHeight;
    const active = document.activeElement, focusItem = active.closest('.vertex');
    const previousKey = focusItem ? focusItem.dataset.name || centerKey : null;
    const previousControl = active.dataset.control;
    const q = el('map-filter').value.trim().toLowerCase();
    const nodes = state.nodes.filter(n => (statusFilter === 'all' || statusOf(n) === statusFilter)
      && mapGroups.matches(n) && hostMatches(n, q));
    vertices.replaceChildren();
    compactLayout = host.clientWidth < 560;
    const core = {name: 'pto-center', enrolled: true, online: true, status: state.centerStatus, statusAt: state.centerStatusAt};
    graph = [card(core, centerKey), ...nodes.map(n => card(n, n.name))];
    layout();
    el('map-count').textContent = nodes.length ? nodes.length + ' 台 Agent' :
        (state.nodes.length ? '没有匹配的主机' : '暂无 Agent');
    if (followFit) fit(); else draw();
    if (focusOnShow) {
      const target = graph.find(n => n.key === focusOnShow);
      focusOnShow = null;
      if (target) {
        const f = frame();
        scale = Math.max(scale, .95);
        tx = (f.w - target.w * scale) / 2 - target.x * scale;
        ty = f.top + (f.h - target.h * scale) / 2 - target.y * scale;
        followFit = false;
        draw();
        target.item.querySelector('button').focus({preventScroll: true});
        return;
      }
    }
    if (previousKey && previousControl) {
      const item = graph.find(n => n.key === previousKey)?.item;
      (item?.querySelector('[data-control="' + previousControl + '"]') || item?.querySelector('button'))?.focus({preventScroll: true});
    }
  }
  function setFilter(value) {
    statusFilter = value;
    for (const b of el('connection-filter').children) b.setAttribute('aria-pressed', String(b.dataset.status === value));
    filterChanged();
  }
  function filterChanged() { followFit = true; render(); }
  el('connection-filter').onclick = e => { if (e.target.dataset.status) setFilter(e.target.dataset.status); };
  el('map-filter').oninput = filterChanged;
  el('map-fit').onclick = fit;
  el('zoom-in').onclick = () => zoom(scale * 1.25);
  el('zoom-out').onclick = () => zoom(scale / 1.25);
  host.onwheel = e => {
    e.preventDefault();
    const rect = host.getBoundingClientRect();
    zoom(scale * (e.deltaY < 0 ? 1.12 : 1 / 1.12), e.clientX - rect.left, e.clientY - rect.top);
  };
  host.onkeydown = e => {
    if (e.target !== host) return;
    const steps = {ArrowLeft: [45, 0], ArrowRight: [-45, 0], ArrowUp: [0, 45], ArrowDown: [0, -45]};
    if (steps[e.key]) {
      const [dx, dy] = steps[e.key];
      tx += dx; ty += dy; followFit = false; draw();
    } else if (e.key === '+' || e.key === '=') zoom(scale * 1.25);
    else if (e.key === '-') zoom(scale / 1.25);
    else if (e.key === 'Home') fit();
    else return;
    e.preventDefault();
  };
  function point(e) {
    const box = host.getBoundingClientRect();
    return {x: e.clientX - box.left, y: e.clientY - box.top};
  }
  host.onpointerdown = e => {
    if (e.button !== 0 || e.target.closest('.vertex')) return;
    host.setPointerCapture(e.pointerId);
    pointers.set(e.pointerId, point(e));
    host.classList.add('dragging');
  };
  host.onpointermove = e => {
    const old = pointers.get(e.pointerId);
    if (!old) return;
    const next = point(e);
    pointers.set(e.pointerId, next);
    const other = [...pointers].find(([id]) => id !== e.pointerId)?.[1];
    if (other) {
      const before = Math.hypot(old.x - other.x, old.y - other.y);
      if (before > 1) zoom(scale * Math.hypot(next.x - other.x, next.y - other.y) / before, other.x, other.y);
    } else {
      tx += next.x - old.x; ty += next.y - old.y; followFit = false; draw();
    }
  };
  host.onlostpointercapture = e => {
    pointers.delete(e.pointerId);
    if (!pointers.size) host.classList.remove('dragging');
  };
  new ResizeObserver(() => {
    if (host.clientWidth && (host.clientWidth !== lastWidth || host.clientHeight !== lastHeight)) {
      followFit = true;
      render();
    }
  }).observe(host);
  return {
    filterChanged,
    redraw: render,
    update(next) {
      state = next;
      // Age can cross the stale threshold even if the server's snapshot has not changed.
      const signature = JSON.stringify([next.nodes, next.centerStatus, next.centerStatusAt, next.centerUpdate?.running, Math.floor(Date.now() / 15000)]);
      if (signature !== lastState || focusOnShow) { lastState = signature; render(); }
    },
    showHost(name) {
      expanded.add(name); saveModes();
      mapGroups.clear();
      el('map-filter').value = '';
      focusOnShow = name;
      setFilter('all');
      location.hash = 'fleet';
      if (!el('fleet').hidden) render();
    }
  };
}
