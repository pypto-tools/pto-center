'use strict';

function createWorkflowPage() {
  const templates = el('wf-template'), runs = el('wf-run'), hosts = el('wf-node');
  const selected = new Set();
  let catalog = {templates: [], runs: [], more: false}, template, detail;
  let cursors = [0], catalogRequest = 0, detailRequest = 0, pendingStart, starting = false;

  function renderTargets() {
    const query = el('wf-filter').value;
    const enrolled = state.nodes.filter(n => n.enrolled);
    const valid = new Set(enrolled.map(n => n.name));
    for (const name of selected) if (!valid.has(name)) selected.delete(name);
    const visible = enrolled.filter(n => hostMatches(n, query));
    const target = el('wf-targets'), focused = document.activeElement;
    const focusedName = target.contains(focused) ? focused.value : null;
    const rows = visible.map(n => {
      const label = groupChoice(n.name, selected.has(n.name), checked => {
        if (checked) selected.add(n.name); else selected.delete(n.name);
        selectionSummary();
      });
      label.firstChild.disabled = starting;
      label.append(make('small', groupText(n)), connectionBadge(n));
      return label;
    });
    if (!rows.length) rows.push(make('p', enrolled.length ? '没有符合筛选条件的主机' : '尚无已接入的主机'));
    target.replaceChildren(...rows);
    if (focusedName) [...target.querySelectorAll('input')].find(i => i.value === focusedName)?.focus({preventScroll: true});
    selectionSummary();
  }
  function selectionSummary() {
    const hidden = state.nodes.filter(n => selected.has(n.name) && !hostMatches(n, el('wf-filter').value)).length;
    const summary = el('wf-selection');
    summary.textContent = '已选 ' + selected.size + ' 台' + (hidden ? '，其中 ' + hidden + ' 台在筛选外' : '');
    summary.title = [...selected].sort().join('、');
    el('wf-start').disabled = !template || !selected.size || starting || state.role !== 'admin';
  }
  function renderTemplate(chosenByUser = false) {
    const current = catalog.templates.find(w => w.id === templates.value);
    if (template && (template.id !== current?.id || template.revision !== current?.revision)) {
      if (!chosenByUser) selected.clear();
      pendingStart = null;
    }
    template = current;
    el('wf-empty').hidden = !!template;
    el('wf-plan').hidden = !template;
    el('wf-edit').disabled = !template || starting;
    if (!template) return;
    el('wf-revision').textContent = '修订 ' + template.revision;
    el('wf-steps').replaceChildren(...template.steps.map(step => {
      const row = make('li', step.tool);
      row.append(make('small', step.version));
      if (step.config && Object.keys(step.config).length) {
        const release = state.tools.find(t => t.digest === step.bundle);
        const schema = release?.schema || {};
        const items = make('ul');
        items.className = 'wf-config-summary';
        for (const [key, value] of Object.entries(step.config)) {
          const title = fieldSchema(schema, schema.properties?.[key]).title || key;
          items.append(make('li', title + ' = ' + display(value)));
        }
        row.append(items);
      }
      return row;
    }));
    renderTargets();
  }
  function renderReceipts() {
    if (!detail) return;
    const name = hosts.value, steps = detail.workflow.steps;
    const receipts = detail.receipts;
    const byAction = new Map(receipts.map(r => [r.step + '/' + (r.action || 'install'), r]));
    const active = ['queued', 'dispatched', 'progress'];
    const stopped = receipts.some(r => !r.verified && !active.includes(r.state));
    function operationRow(step, i, action) {
      const r = byAction.get((i + 1) + '/' + action), config = action === 'apply-config';
      let status;
      if (!r) status = badge(stopped ? '未执行' : '等待前一步');
      else if (r.verified && config) status = badge(r.state === 'skipped' ? '配置已满足' : '配置已生效', 'ok');
      else if (r.verified) status = badge(r.state === 'skipped' ? '原已安装' : '已安装', 'ok');
      else if (r.state === 'succeeded' || r.state === 'skipped') status = badge(config ? '配置待核对' : '版本待核对', 'wait');
      else status = badge(statuses[r.state] || r.state, taskTones[r.state]);
      return [String(i + 1) + (config ? ' · 配置' : ' · 安装'), stack(step.tool, step.version), status, r?.summary || '—'];
    }
    const rows = [];
    let verified = 0;
    steps.forEach((step, i) => {
      const actions = step.config && Object.keys(step.config).length ? ['install', 'apply-config'] : ['install'];
      for (const action of actions) rows.push(operationRow(step, i, action));
      if (actions.every(action => byAction.get((i + 1) + '/' + action)?.verified)) verified++;
    });
    el('wf-progress').textContent = name + ' · ' + verified + '/' + steps.length +
      ' 步已核实' + (stopped ? ' · 已停止' : '');
    table(el('wf-results'), ['步骤', '软件和版本', '状态', '回执'], rows);
  }
  async function loadRun() {
    const id = runs.value, request = ++detailRequest;
    const node = detail?.id === id ? hosts.value : '';
    if (detail?.id !== id) {
      detail = null;
      selectOptions(hosts, []);
    }
    if (!detail || detail.node !== node) {
      el('wf-results').replaceChildren();
      el('wf-progress').textContent = id ? '正在读取回执' : '';
    }
    if (!id) return;
    const params = new URLSearchParams({id});
    if (node) params.set('node', node);
    const result = await api('workflow-run?' + params);
    if (request !== detailRequest || runs.value !== id) return;
    detail = result;
    selectOptions(hosts, detail.nodes.map(n => [n, n]), detail.node);
    renderReceipts();
  }
  async function refresh(next = cursors, preferredTemplate, preferredRun) {
    const request = ++catalogRequest;
    const cursor = next.at(-1);
    const data = await api('workflows' + (cursor ? '?before=' + cursor : ''));
    if (request !== catalogRequest) return;
    catalog = data;
    cursors = next;
    selectOptions(templates, catalog.templates.map(w => [w.id, w.name]), preferredTemplate || templates.value);
    if (starting) templates.disabled = true;
    renderTemplate();
    selectOptions(runs, catalog.runs.map(r =>
      [r.id, r.name + ' · 修订 ' + r.revision + ' · ' + dateText(r.created) + ' · ' + r.nodeCount + ' 台 · ' + r.id.slice(0, 8)]),
      preferredRun || runs.value);
    el('wf-no-runs').hidden = !!runs.value;
    el('wf-history').hidden = !runs.value;
    el('wf-pages').hidden = !catalog.more && cursors.length === 1;
    el('wf-newer').disabled = cursors.length === 1;
    el('wf-older').disabled = !catalog.more;
    await loadRun();
  }
  async function start() {
    if (el('wf-start').disabled) return;
    const input = {workflow: template.id, revision: template.revision, nodes: [...selected].sort()};
    const key = JSON.stringify(input);
    if (pendingStart?.key !== key) {
      const bytes = crypto.getRandomValues(new Uint8Array(32));
      pendingStart = {key, id: Array.from(bytes, n => n.toString(16).padStart(2, '0')).join('')};
    }
    starting = true;
    templates.disabled = true;
    renderTargets();
    try {
      const run = await api('workflow-run', {...input, requestId: pendingStart.id});
      pendingStart = null;
      selected.clear();
      notice('已向 ' + input.nodes.length + ' 台主机下发工作流，可在运行记录中查看进度。');
      await refresh([0], input.workflow, run.id);
    } finally {
      starting = false;
      templates.disabled = !catalog.templates.length;
      renderTemplate();
    }
  }
  function edit(existing) {
    const body = modal(existing ? '编辑工作流' : '创建工作流');
    const releases = state.tools.filter(t => t.manifest.capabilities.includes('install'));
    if (!releases.length) {
      const link = make('a', '前往工具与配置');
      link.href = '#tools';
      link.onclick = () => el('dialog').close();
      body.append(make('p', '请先注册支持首次安装的工具发布包。'), link);
      return;
    }
    const form = make('form'), name = make('input');
    name.id = 'wf-name';
    name.required = true;
    name.maxLength = 100;
    name.value = existing?.name || '';
    const label = make('label', '工作流名称');
    label.htmlFor = name.id;
    const list = make('ol'), steps = [];
    list.className = 'wf-edit-steps';
    const save = make('button', '保存模板');
    save.type = 'submit';
    save.className = 'primary';
    const error = make('p');
    error.className = 'wf-error';
    error.setAttribute('role', 'alert');
    const add = button('添加步骤', () => {
      const used = new Set(steps.map(s => s.release.manifest.id));
      const next = releases.find(t => !used.has(t.manifest.id)) || releases[0];
      steps.push(editorStep(next.digest, {}));
      draw();
      steps.at(-1).choice.focus();
    });
    function editorStep(digest, saved) {
      const li = make('li'), row = make('div'), choice = make('select'), config = make('div');
      row.className = 'wf-edit-row';
      selectOptions(choice, releases.map(t => [t.digest, t.manifest.id + ' · ' + t.version]), digest);
      const step = {li, choice, fields: [], release: null};
      const drafts = new Map();
      function selectRelease() {
        if (step.release) drafts.set(step.release.digest, step.fields);
        const previous = step.release;
        const release = releases.find(t => t.digest === choice.value);
        step.release = release;
        config.replaceChildren();
        const schema = release.schema;
        if (!schema || !release.manifest.capabilities.includes('apply-config')) {
          step.fields = [];
          config.append(make('small', '此发布包仅安装软件'));
          return;
        }
        const fields = make('fieldset');
        fields.className = 'wf-step-config';
        fields.append(make('legend', '安装后下发配置'));
        if (drafts.has(release.digest)) {
          step.fields = drafts.get(release.digest);
        } else {
          const compatible = previous?.manifest.id === release.manifest.id &&
              previous.manifest.configSchemaVersion === release.manifest.configSchemaVersion;
          const prior = new Map((compatible ? step.fields : []).map(f => [f.key, f]));
          step.fields = Object.entries(schema.properties || {}).map(([key, prop]) => {
            const s = fieldSchema(schema, prop), field = schemaField(key, s);
            if (prior.has(key)) {
              field.toggle.checked = prior.get(key).toggle.checked;
              field.input.value = prior.get(key).input.value;
            } else if (!previous && Object.hasOwn(saved, key)) {
              field.toggle.checked = true;
              setValue(field.input, s, saved[key]);
            } else if (s.default !== undefined) {
              setValue(field.input, s, s.default);
            }
            field.input.oninput = () => { field.toggle.checked = true; };
            return field;
          });
        }
        fields.append(...step.fields.map(f => f.wrap));
        config.append(fields);
      }
      function move(by) {
        const i = steps.indexOf(step);
        [steps[i], steps[i + by]] = [steps[i + by], steps[i]];
        draw();
        choice.focus();
      }
      step.up = button('上移', () => move(-1));
      step.down = button('下移', () => move(1));
      row.append(choice, step.up, step.down, button('移除', () => { steps.splice(steps.indexOf(step), 1); draw(); }));
      li.append(row, config);
      choice.onchange = selectRelease;
      selectRelease();
      return step;
    }
    function draw() {
      list.replaceChildren(...steps.map((s, i) => {
        s.choice.setAttribute('aria-label', '第 ' + (i + 1) + ' 步的软件版本');
        s.up.disabled = i === 0;
        s.down.disabled = i === steps.length - 1;
        return s.li;
      }));
      add.disabled = steps.length >= 500;
      save.disabled = !steps.length;
    }
    for (const s of existing?.steps || [{bundle: releases[0].digest}]) steps.push(editorStep(s.bundle, s.config || {}));
    draw();
    form.append(label, name, make('h3', '安装顺序'), list, add, error, save);
    body.append(form);
    name.focus();
    form.onsubmit = async event => {
      event.preventDefault();
      if (save.disabled) return;
      save.disabled = true;
      error.textContent = '';
      try {
        const saved = await api('workflows', {
          id: existing?.id, revision: existing?.revision, name: name.value.trim(),
          steps: steps.map((s, i) => {
            try { return {bundle: s.choice.value, config: values(s.fields)}; }
            catch(e) { throw Error('第 ' + (i + 1) + ' 步 ' + s.release.manifest.id + '，' + e.message); }
          })
        });
        if (form.isConnected && el('dialog').open) el('dialog').close();
        await refresh(cursors, saved.id);
        notice('已保存「' + saved.name + '」修订 ' + saved.revision);
      } catch (e) {
        error.textContent = e.message;
        notice(e.message);
      } finally {
        save.disabled = !steps.length;
      }
    };
  }
  templates.onchange = () => renderTemplate(true);
  runs.onchange = () => tryRun(loadRun);
  hosts.onchange = () => tryRun(loadRun);
  el('wf-filter').oninput = renderTargets;
  el('wf-start').onclick = () => tryRun(start);
  el('wf-new').onclick = () => edit();
  el('wf-edit').onclick = () => edit(template);
  el('wf-older').onclick = () => tryRun(() => refresh([...cursors, catalog.runs.at(-1).seq]));
  el('wf-newer').onclick = () => tryRun(() => refresh(cursors.slice(0, -1)));
  return {
    refresh,
    forNode(name) {
      selected.clear();
      selected.add(name);
      pendingStart = null;
      el('wf-filter').value = '';
      location.hash = 'workflows';
    }
  };
}
