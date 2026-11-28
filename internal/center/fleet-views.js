'use strict';

// Views share the registered Agent graph. They change the facts displayed on
// its cards, never the management edges or the underlying host selection.
function versionCard(node, release) {
  const o = observation(node.name, release.manifest.id);
  const facts = [['目标版本', release.version]];
  if (o?.seen) facts.push(['最近盘点', dateText(o.seen)]);
  if (!node.enrolled || !o || o.presence !== 'present') {
    const text = !node.enrolled ? '未接入' : !o ? '尚无盘点' : o.presence === 'absent' ? '未安装' : '安装状态未知';
    return {text, facts, tone: 'neutral'};
  }
  const installed = o.installedVersion || '未知', running = o.runningVersion || '未知';
  const active = o.installedVersion === release.version && o.runningVersion === release.version;
  const text = active ? '已运行 ' + running : '已装 ' + installed + ' · 运行 ' + running;
  facts.push(['已安装', installed], ['正在运行', running]);
  if (o.writtenRevision) facts.push(['配置写入修订', String(o.writtenRevision)], ['配置生效修订', String(o.effectiveRevision || '未知')]);
  return {text: node.online ? text : '离线快照 · ' + text, facts, tone: active ? 'ok' : 'wait'};
}
function stageName(r, m) {
  return (m.stage === 1 ? '第一批 · ' + r.firstGroup : '第二批 · ' + r.nextGroup) + (m.shared ? '（共用）' : '');
}
function memberLabel(m) {
  if (m.excluded) return '未纳入 · ' + m.excluded;
  if (m.blocked) return '待核对 · ' + m.blocked;
  if (m.proof && m.atTarget) {
    if (m.proof !== m.task) return '已核对生效';
    return m.state === 'skipped' ? '原已生效' : '本轮已成功';
  }
  if (m.proof) return '目标版本需核对';
  if (m.state === 'succeeded') return '回执成功 · 待核实版本';
  return m.state === 'waiting' ? '待发布' : statuses[m.state] || m.state;
}
function showRolloutReceipt(r, m) {
  const root = modal(m.node + ' · 更新任务');
  const fields = make('dl');
  fields.className = 'receipt-fields';
  const rows = [['目标', r.tool + ' ' + r.version], ['所属批次', stageName(r, m)],
    ['任务 ID', m.task], ['任务状态', statuses[m.state] || m.state]];
  if (m.summary) rows.push(['原始回执', m.summary]);
  if (m.proof) rows.push(['版本核对', memberLabel(m)]);
  if (m.proof && m.proof !== m.task) rows.push(['核对盘点 ID', m.proof]);
  for (const [label, value] of rows) fields.append(make('dt', label), make('dd', value));
  root.append(fields);
}
function createFleetViews(redraw) {
  const view = el('map-view'), releases = el('map-release'), rounds = el('map-round');
  const remaining = el('round-remaining'), summary = el('view-summary');
  const initial = new URL(location.href).searchParams.get('view');
  if (initial === 'versions' || initial === 'rollouts') view.value = initial;
  let snapshot, release, round, members = new Map(), serial = 0, requestedRound = '', lastMode = '';

  function clearRound() {
    round = null; members = new Map();
    remaining.hidden = true;
  }
  function progress(r) {
    const counts = [1, 2].map(stage => {
      const group = r.members.filter(m => m.stage === stage && !m.excluded);
      return group.filter(m => m.proof && m.atTarget).length + '/' + group.length;
    });
    return '第一批 ' + counts[0] + ' 已核实 · 第二批 ' + counts[1] + ' 已核实';
  }
  async function update(next) {
    snapshot = next;
    const seq = ++serial, mode = view.value;
    const switched = mode !== lastMode;
    lastMode = mode;
    const versions = mode === 'versions', staging = mode === 'rollouts';
    el('view-controls').hidden = mode === 'connections';
    releases.hidden = !versions;
    rounds.hidden = !staging || !next.rollouts?.length;
    el('round-new').hidden = !staging || next.role !== 'admin';
    if (!staging) clearRound();
    if (mode === 'connections') { if (switched) redraw(); return; }
    if (versions) {
      selectOptions(releases, next.tools.map(t => [t.digest, t.manifest.id + ' · ' + t.version]));
      release = next.tools.find(t => t.digest === releases.value);
      releases.disabled = !release;
      const confirmed = release && next.nodes.filter(n => {
        const o = observation(n.name, release.manifest.id);
        return n.enrolled && o?.presence === 'present' && o.installedVersion === release.version && o.runningVersion === release.version;
      }).length;
      summary.textContent = release ? '最近盘点有 ' + confirmed + ' 台已运行目标版本' : '尚无已注册的发布包';
    } else if (staging) {
      const entries = (next.rollouts || []).map(r => [r.id, r.tool + ' ' + r.version + ' · ' + r.firstGroup + ' → ' + r.nextGroup + ' · ' + dateText(r.created) + ' · ' + r.id.slice(0, 6)]);
      selectOptions(rounds, entries, requestedRound || rounds.value);
      requestedRound = '';
      const id = rounds.value;
      if (round?.id !== id) {
        clearRound();
        summary.textContent = id ? '正在读取本轮回执' : '暂无发布轮次';
        redraw();
      }
      if (id) {
        try {
          const fresh = await api('rollout?id=' + encodeURIComponent(id));
          if (seq !== serial) return;
          round = fresh;
          members = new Map(fresh.members.map(m => [m.node, m]));
          summary.textContent = progress(round);
          remaining.hidden = false;
          remaining.textContent = '第二批剩余 ' + round.remaining + ' 台';
        } catch (error) {
          if (seq !== serial) return;
          clearRound();
          summary.textContent = '轮次读取失败';
          redraw();
          throw error;
        }
      }
    }
    redraw();
  }
  function changeView() {
    ++previewGeneration;
    const url = new URL(location.href);
    if (view.value === 'connections') url.searchParams.delete('view'); else url.searchParams.set('view', view.value);
    history.replaceState(history.state, '', url);
    if (snapshot) return update(snapshot);
  }
  view.onchange = () => tryRun(changeView);
  releases.onchange = rounds.onchange = () => { if (snapshot) tryRun(() => update(snapshot)); };

  function nodeCard(node, core) {
    if (view.value === 'versions' && release) {
      return core ? {text: '目标版本 ' + release.version, facts: [['工具', release.manifest.id]], tone: 'neutral'} : versionCard(node, release);
    }
    if (view.value !== 'rollouts' || !round) return null;
    if (core) return {text: round.firstGroup + ' → ' + round.nextGroup, facts: [['目标', round.tool + ' ' + round.version], ['进度', progress(round)]], tone: 'neutral'};
    const m = members.get(node.name);
    if (!m) return {text: '本轮未选中', facts: [], tone: 'neutral'};
    const label = memberLabel(m), facts = [['所属批次', stageName(round, m)], ['本轮状态', label]];
    const tone = m.proof && m.atTarget ? 'ok' : ['failed','conflict','unknown'].includes(m.state) ? 'error' : m.excluded ? 'neutral' : 'wait';
    const receipt = m.task && {summary: m.summary, open: () => showRolloutReceipt(round, m)};
    return {text: (m.stage === 1 ? '第一批' : '第二批') + ' · ' + label, facts, tone, receipt};
  }
  function showRemaining() {
    const r = round;
    if (!r) return;
    const root = modal('第二批 · ' + r.nextGroup);
    const rows = r.members.filter(m => m.stage === 2 || m.shared).map(m => [
      m.node, stageName(r, m), m.shared && !m.excluded ? '已计入第一批 · ' + memberLabel(m) : memberLabel(m)
    ]);
    root.append(make('p', r.tool + ' ' + r.version + ' · 剩余可发布 ' + r.remaining + ' 台'));
    const tbl = make('table');
    table(tbl, ['主机', '所属批次', '本轮状态'], rows);
    const scroll = make('div'); scroll.className = 'scroll'; scroll.append(tbl); root.append(scroll);
    if (!r.firstReady && r.remaining) root.append(make('p', '第一批全部核实运行目标版本后可以发布第二批。可在「工具与配置」对该版本刷新盘点。'));
    if (snapshot.role === 'admin' && r.firstReady && r.remaining) {
      root.append(button('预览第二批', () => previewStep({round:r.id})));
    }
  }
  function newRound() {
    const root = modal('新建分批发布');
    const tools = snapshot.tools.filter(t => t.manifest.capabilities.includes('update'));
    const groups = [...new Set(snapshot.nodes.flatMap(n => n.groups))].sort();
    if (!tools.length || groups.length < 2) {
      root.append(make('p', !tools.length ? '请先注册支持更新的工具。' : '请先在主机清单中设置两个分组。'));
      return;
    }
    function field(text, entries, selected) {
      const label = make('label', text), select = make('select');
      selectOptions(select, entries, selected);
      label.append(select); root.append(label);
      return select;
    }
    const groupOptions = groups.map(g => [g, g]);
    const bundle = field('工具和目标版本', tools.map(t => [t.digest, t.manifest.id + ' · ' + t.version]), releases.value);
    const first = field('第一批分组', groupOptions, groups.includes('CI') ? 'CI' : groups[0]);
    const next = field('第二批分组', groupOptions, groups.includes('User') && first.value !== 'User' ? 'User' : groups.find(g => g !== first.value));
    const submit = button('预览第一批', () => previewStep({bundle:bundle.value, firstGroup:first.value, nextGroup:next.value}));
    first.onchange = next.onchange = () => { submit.disabled = first.value === next.value; };
    root.append(submit);
  }
  async function previewStep(request) {
    const generation = ++previewGeneration;
    const plan = await api('rollout-preview', request);
    if (generation !== previewGeneration || !el('dialog').open || view.value !== 'rollouts') return;
    const r = plan.rollout, step = r.stage === 1 ? '第一批' : '第二批';
    const root = modal('确认' + step + '更新');
    const selected = new Set(plan.tasks.map(t => t.node));
    root.append(make('p', r.tool + ' ' + r.version + ' · ' + (r.stage === 1 ? r.firstGroup : r.nextGroup) + ' · 本次 ' + selected.size + ' 台'));
    const tbl = make('table'), scroll = make('div');
    table(tbl, ['主机', '所属批次', '安排'], r.members.map(m => [
      m.node, stageName(r, m), selected.has(m.node) ? '本次发布' : memberLabel(m)
    ]));
    scroll.className = 'scroll'; scroll.append(tbl); root.append(scroll);
    const send = button('确认发布' + step + '（' + selected.size + ' 台）', async () => {
      send.disabled = true;
      try {
        await api('publish', {id: plan.id});
        requestedRound = r.id;
        el('dialog').close();
        await refresh();
      } finally { send.disabled = false; }
    });
    send.className = 'primary'; root.append(send);
  }
  el('round-new').onclick = () => tryRun(newRound);
  remaining.onclick = () => tryRun(showRemaining);
  return {update, nodeCard};
}
