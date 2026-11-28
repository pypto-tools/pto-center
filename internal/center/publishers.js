'use strict';
const publisherPage = (() => {
  const checking = new Set();
  const status = {pending: ['待验证发布包', 'wait'], verified: ['签名已验证', 'ok'], failed: ['验证失败', 'error']};
  function link(text, href) {
    const a = make('a', text);
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    return a;
  }
  function field(label, input) {
    const wrapper = make('label', label);
    wrapper.append(input);
    return wrapper;
  }
  function render(rows, admin) {
    const list = el('publisher-list');
    el('publisher-empty').hidden = rows.length > 0;
    list.hidden = !rows.length;
    const titles = ['工具 / 仓库', '验证状态', '最近验签通过', '检查时间'];
    if (admin) titles.push('操作');
    table(list, titles, rows.map(k => {
      const source = make('div');
      source.className = 'stack';
      source.append(make('strong', k.tool), link(k.repository, 'https://github.com/' + k.repository));
      const result = make('div');
      result.className = 'stack';
      result.append(badge(...(status[k.state] || status.pending)));
      if (k.message) result.append(make('small', k.message));
      const version = make('span', k.version || '—');
      if (k.digest) version.title = 'SHA-256 ' + k.digest;
      const cells = [source, result, version, k.checked ? new Date(k.checked * 1000).toLocaleString('zh-CN') : '—'];
      if (admin) {
        const actions = make('div');
        const check = button(checking.has(k.tool) ? '验证中…' : '验证 GitHub 发布', async () => {
          checking.add(k.tool);
          check.disabled = true;
          check.textContent = '验证中…';
          try {
            const checked = await api('check-tool-release', {tool: k.tool});
            notice(checked.state === 'verified' ? k.tool + ' ' + checked.version + ' 已验签并登记' : checked.message);
          } finally {
            checking.delete(k.tool);
            check.disabled = false;
            check.textContent = '验证 GitHub 发布';
            await refresh();
          }
        });
        check.disabled = checking.has(k.tool);
        actions.append(check, button('密钥详情', () => details(k)));
        cells.push(actions);
      }
      return cells;
    }));
  }
  function details(k) {
    const root = modal(k.tool + ' · 发布密钥');
    root.append(make('p', '公钥指纹 SHA-256'), make('pre', k.fingerprint),
      make('p', '登记的 Ed25519 公钥'), make('pre', k.publicKey),
      link('打开 GitHub 发布环境设置', 'https://github.com/' + k.repository + '/settings/environments'),
      button('更换发布密钥', () => open(k)));
  }
  function open(current) {
    const root = modal(current ? '更换发布密钥' : '接入工具');
    const tool = make('input'), repo = make('input'), imported = make('input');
    tool.placeholder = 'pto-system-doctor';
    repo.placeholder = 'pypto-tools/pto-system-doctor';
    tool.value = current?.tool || '';
    repo.value = current?.repository || '';
    tool.disabled = repo.disabled = Boolean(current);
    root.append(field('工具 ID', tool), field('GitHub 仓库', repo));
    if (current) root.append(make('p', '新版本将使用新公钥。已登记版本继续使用各自发布时的公钥。'));
    function identity() {
      const id = tool.value.trim(), repository = repo.value.trim();
      if (!/^[a-z][a-z0-9-]{0,62}$/.test(id) || ['pto-agent', 'pto-center'].includes(id) ||
          !/^[A-Za-z0-9_-]+\/[A-Za-z0-9_-][A-Za-z0-9_.-]*$/.test(repository)) {
        throw Error('请填写业务工具 ID 和 GitHub owner/repository');
      }
      return {tool: id, repository};
    }
    const generate = button('生成工具密钥', async () => {
      const k = identity();
      generate.disabled = true;
      try {
        let jwk;
        try {
          const pair = await crypto.subtle.generateKey('Ed25519', true, ['sign', 'verify']);
          jwk = await crypto.subtle.exportKey('jwk', pair.privateKey);
        } catch {
          throw Error('当前浏览器无法生成 Ed25519 密钥。请使用受信任的 HTTPS 和新版浏览器，或登记用 pto-center keygen 生成的公钥。');
        }
        if (!el('dialog').open || !generate.isConnected) return;
        const fromURL = s => atob(s.replace(/-/g, '+').replace(/_/g, '/'));
        const pub = fromURL(jwk.x), seed = fromURL(jwk.d);
        // Go's Ed25519 private-key file contains the 32-byte seed followed by the public key.
        stage(root, {...k, publicKey: btoa(pub)}, btoa(seed + pub), current);
      } finally { generate.disabled = false; }
    });
    const useExisting = make('details');
    useExisting.append(make('summary', '登记已有公钥'), field('release.pub 内容', imported),
      button('使用此公钥', () => {
        const k = identity(), publicKey = imported.value.trim();
        if (!/^[A-Za-z0-9+/]{43}=$/.test(publicKey)) throw Error('请填写 Base64 编码的 32 字节 Ed25519 公钥');
        stage(root, {...k, publicKey}, '', current);
      }));
    root.append(generate, useExisting);
  }
  function stage(root, k, privateValue, current) {
    root.replaceChildren(make('p', k.tool + ' · ' + k.repository));
    let saved;
    if (privateValue) {
      const secret = make('input');
      secret.type = 'password';
      secret.readOnly = true;
      secret.autocomplete = 'off';
      secret.value = privateValue;
      privateValue = '';
      const show = button('显示', () => {
        secret.type = secret.type === 'password' ? 'text' : 'password';
        show.textContent = secret.type === 'password' ? '显示' : '隐藏';
      });
      const copy = button('复制私钥', async () => {
        try { await navigator.clipboard.writeText(secret.value); copy.textContent = '已复制'; }
        catch { secret.type = 'text'; show.textContent = '隐藏'; secret.select(); throw Error('请复制已选中的私钥'); }
      });
      root.append(make('p', '把私钥保存到此仓库的 release 环境，Secret 名称为 PTO_SIGNING_KEY。关闭窗口后无法再次查看。'),
        link('打开 GitHub 环境设置', 'https://github.com/' + k.repository + '/settings/environments'),
        field('PTO_SIGNING_KEY', secret), show, copy);
      saved = make('input');
      saved.type = 'checkbox';
      root.append(field('我已保存私钥', saved));
      el('dialog').addEventListener('close', () => { secret.value = ''; }, {once: true});
    }
    root.append(make('p', '将登记的公钥'), make('pre', k.publicKey));
    const save = button('登记公钥', async () => {
      save.disabled = true;
      try {
        await api('tool-publisher', {...k, expectedFingerprint: current?.fingerprint || ''});
        el('dialog').close();
        notice('公钥已登记。仓库发布正式版本后，可点击「验证 GitHub 发布」。');
        await refresh();
      } finally { save.disabled = Boolean(saved && !saved.checked); }
    });
    if (saved) {
      save.disabled = true;
      saved.onchange = () => { save.disabled = !saved.checked; };
    }
    root.append(save);
  }
  return {open, render};
})();
