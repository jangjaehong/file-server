'use strict';
// Single page UI. Public: program list/detail/downloads. Admin: programs, releases, uploads, tokens.

const $app = document.getElementById('app');
const PLATFORMS = ['windows-amd64', 'windows-386', 'windows-arm64', 'linux-amd64', 'linux-arm64', 'linux-arm', 'darwin-amd64', 'darwin-arm64', 'any'];
let me = null; // logged-in admin

// ---------------------------------------------------------------- utilities
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
const enc = encodeURIComponent;
const fmtSize = (n) => { const u = ['B', 'KB', 'MB', 'GB', 'TB']; let i = 0; while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; } return (i ? n.toFixed(1) : n) + ' ' + u[i]; };
const fmtDate = (s) => s ? new Date(s).toLocaleString('ko-KR', { dateStyle: 'medium', timeStyle: 'short' }) : '-';
const statusKo = { draft: '초안', published: '배포중', yanked: '회수됨' };

async function api(method, path, body) {
  const opt = { method, headers: { 'X-Requested-With': 'fetch' }, credentials: 'same-origin' };
  if (body !== undefined) { opt.headers['Content-Type'] = 'application/json'; opt.body = JSON.stringify(body); }
  const res = await fetch(path, opt);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

// Minimal, safe Markdown: headings, lists, bold/italic, code, links, paragraphs.
function md(src) {
  const inline = (t) => esc(t)
    .replace(/`([^`]+)`/g, '<code>$1</code>')
    .replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>')
    .replace(/(^|[^*])\*([^*]+)\*/g, '$1<em>$2</em>')
    .replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noopener">$1</a>');
  const out = []; let list = null, para = [], code = null;
  const flushPara = () => { if (para.length) { out.push('<p>' + para.map(inline).join('<br>') + '</p>'); para = []; } };
  const flushList = () => { if (list) { out.push(`<${list.tag}>` + list.items.map((i) => '<li>' + inline(i) + '</li>').join('') + `</${list.tag}>`); list = null; } };
  for (const line of String(src || '').replace(/\r/g, '').split('\n')) {
    if (code !== null) { if (/^```/.test(line)) { out.push('<pre>' + esc(code.join('\n')) + '</pre>'); code = null; } else code.push(line); continue; }
    if (/^```/.test(line)) { flushPara(); flushList(); code = []; continue; }
    let m;
    if ((m = line.match(/^(#{1,3})\s+(.*)/))) { flushPara(); flushList(); out.push(`<h${m[1].length}>${inline(m[2])}</h${m[1].length}>`); continue; }
    if ((m = line.match(/^\s*[-*+]\s+(.*)/)) || (m = line.match(/^\s*\d+[.)]\s+(.*)/))) {
      flushPara(); const tag = /^\s*\d/.test(line) ? 'ol' : 'ul';
      if (!list || list.tag !== tag) { flushList(); list = { tag, items: [] }; }
      list.items.push(m[1]); continue;
    }
    if (!line.trim()) { flushPara(); flushList(); continue; }
    flushList(); para.push(line);
  }
  if (code !== null) out.push('<pre>' + esc(code.join('\n')) + '</pre>');
  flushPara(); flushList();
  return '<div class="md">' + out.join('') + '</div>';
}

function guessPlatform() {
  const ua = navigator.userAgent;
  if (/Windows/.test(ua)) return /ARM64|aarch64/i.test(ua) ? 'windows-arm64' : 'windows-amd64';
  if (/Mac OS X/.test(ua)) return 'darwin-arm64';
  if (/Linux/.test(ua)) return /aarch64|arm64/i.test(ua) ? 'linux-arm64' : 'linux-amd64';
  return '';
}

function downloadButtons(slug, rel, highlight = true) {
  const mine = guessPlatform();
  return '<div class="dl">' + rel.assets.map((a) => {
    const href = `/download/${enc(slug)}/${enc(rel.version)}/${enc(a.platform)}/${enc(a.filename)}`;
    const primary = highlight && (a.platform === mine || rel.assets.length === 1) ? ' primary' : '';
    return `<a class="btn${primary}" href="${href}" title="SHA-256 ${esc(a.sha256)}">⬇ ${esc(a.platform)} · ${esc(a.filename)} <span class="muted">(${fmtSize(a.size)})</span></a>`;
  }).join('') + '</div>';
}

function assetTable(rel, admin) {
  if (!rel.assets.length) return '<p class="muted">파일 없음</p>';
  return `<table><tr><th>플랫폼</th><th>파일</th><th class="hide-sm">크기</th><th class="hide-sm">SHA-256</th><th>다운로드</th>${admin ? '<th></th>' : ''}</tr>` +
    rel.assets.map((a) => `<tr><td>${esc(a.platform)}</td><td><a href="/files/${a.id}">${esc(a.filename)}</a></td>
      <td class="hide-sm">${fmtSize(a.size)}</td><td class="hide-sm"><code class="hash">${esc(a.sha256)}</code></td><td>${a.downloads}</td>
      ${admin ? `<td>${rel.status === 'draft' ? `<button class="btn small danger" data-act="del-asset" data-id="${a.id}">삭제</button>` : ''}</td>` : ''}</tr>`).join('') + '</table>';
}

const badges = (r) => `<span class="badge ${esc(r.channel)}">${esc(r.channel)}</span>` + (r.mandatory ? ' <span class="badge mandatory">필수 업데이트</span>' : '');

// ---------------------------------------------------------------- public pages
async function pageHome() {
  const progs = await api('GET', '/api/v1/programs');
  $app.innerHTML = `<h1>프로그램</h1><p class="muted">필요한 프로그램을 선택해 최신 버전을 내려받으세요.</p>` +
    (progs.length ? '<div class="grid">' + progs.map((p) => `
      <div class="card"><div class="row spread"><h3><a href="#/p/${enc(p.slug)}">${esc(p.name)}</a></h3>
        ${p.latest_version ? `<span class="badge stable">v${esc(p.latest_version)}</span>` : ''}</div>
        <p class="muted">${esc(p.summary || '')}</p>
        <div class="row">${p.latest_version ? `<a class="btn primary" href="#/p/${enc(p.slug)}">다운로드</a>` : ''}
        ${p.latest_beta ? `<span class="badge beta">beta v${esc(p.latest_beta)}</span>` : ''}</div></div>`).join('') + '</div>'
      : '<div class="card muted">아직 배포된 프로그램이 없습니다.</div>');
}

async function pageProgram(slug) {
  const p = await api('GET', `/api/v1/programs/${enc(slug)}`);
  const L = p.latest;
  let h = `<div class="row spread"><div><h1>${esc(p.name)}</h1><div class="muted">${esc(p.summary)}</div></div>
    ${L ? `<span class="badge stable">최신 v${esc(L.version)}</span>` : ''}</div>`;
  if (L) {
    h += `<div class="card" style="margin-top:16px"><div class="row spread"><h3>최신 버전 v${esc(L.version)}</h3><span class="muted">${fmtDate(L.published_at)}</span></div>
      ${downloadButtons(p.slug, L)}
      <details><summary>고정 다운로드 링크 / 자동 업데이트</summary>
      <p>항상 최신 버전: <code>${esc(location.origin)}/download/${esc(p.slug)}/latest/&lt;platform&gt;</code></p>
      <p>업데이트 확인: <code>${esc(location.origin)}/api/v1/update?program=${esc(p.slug)}&amp;version=현재버전&amp;platform=windows-amd64</code></p></details></div>`;
  }
  if (p.latest_beta) {
    h += `<div class="card"><div class="row spread"><h3>베타 v${esc(p.latest_beta.version)} <span class="badge beta">beta</span></h3><span class="muted">${fmtDate(p.latest_beta.published_at)}</span></div>
      ${downloadButtons(p.slug, p.latest_beta, false)}</div>`;
  }
  if (p.description) h += `<h2>프로그램 설명</h2><div class="card">${md(p.description)}</div>`;
  h += `<h2>패치 노트 / 버전 기록</h2>`;
  h += p.releases.map((r) => `<div class="card" id="v${esc(r.version)}"><div class="row spread"><div class="row"><h3>v${esc(r.version)}</h3>${badges(r)}</div>
      <span class="muted">${fmtDate(r.published_at)}</span></div>
      ${r.notes ? md(r.notes) : '<p class="muted">패치 노트 없음</p>'}
      <details><summary>파일 ${r.assets.length}개</summary>${assetTable(r, false)}</details></div>`).join('') || '<p class="muted">버전 없음</p>';
  $app.innerHTML = h;
}

// ---------------------------------------------------------------- admin pages
async function ensureLogin() {
  if (me) return true;
  try { me = (await api('GET', '/api/v1/auth/me')).user; return true; } catch { me = null; return false; }
}

function pageLogin() {
  $app.innerHTML = `<div class="card" style="max-width:380px;margin:40px auto"><h1>관리자 로그인</h1>
    <form id="login"><label>아이디</label><input type="text" name="username" autocomplete="username" required>
    <label>비밀번호</label><input type="password" name="password" autocomplete="current-password" required>
    <div class="err" id="err"></div><button class="btn primary">로그인</button></form></div>`;
  document.getElementById('login').addEventListener('submit', async (e) => {
    e.preventDefault(); const f = new FormData(e.target);
    try { await api('POST', '/api/v1/auth/login', { username: f.get('username'), password: f.get('password') }); me = null; route(); }
    catch (err) { document.getElementById('err').textContent = err.message; }
  });
}

function adminNav(active) {
  const tab = (h, t) => `<a class="btn${active === h ? ' primary' : ''}" href="#/admin${h}">${t}</a>`;
  return `<div class="row spread" style="margin-bottom:16px"><div class="row">${tab('', '프로그램 관리')}${tab('/tokens', 'API 토큰')}</div>
    <div class="row"><span class="muted">${esc(me)}</span><button class="btn small" data-act="logout">로그아웃</button></div></div>`;
}

async function pageAdmin() {
  const progs = await api('GET', '/api/v1/admin/programs');
  $app.innerHTML = adminNav('') + `<h1>프로그램 관리</h1>
    <table class="card"><tr><th>프로그램</th><th>slug</th><th>최신(stable)</th><th>배포 버전 수</th></tr>
    ${progs.map((p) => `<tr><td><a href="#/admin/p/${enc(p.slug)}">${esc(p.name)}</a></td><td><code>${esc(p.slug)}</code></td>
      <td>${p.latest_version ? 'v' + esc(p.latest_version) : '<span class="muted">-</span>'}</td><td>${p.release_count}</td></tr>`).join('') || '<tr><td colspan=4 class="muted">없음</td></tr>'}</table>
    <h2>새 프로그램 등록</h2>
    <form class="card" id="newprog">
      <label>slug (URL·API 식별자, 변경 불가: 소문자/숫자/-)</label><input type="text" name="slug" placeholder="my-app" required pattern="[a-z0-9][a-z0-9-]*">
      <label>이름</label><input type="text" name="name" placeholder="My App" required>
      <label>한 줄 소개</label><input type="text" name="summary">
      <label>설명 (Markdown)</label><textarea name="description"></textarea>
      <div class="err" id="err"></div><button class="btn primary">등록</button></form>`;
  document.getElementById('newprog').addEventListener('submit', async (e) => {
    e.preventDefault(); const f = Object.fromEntries(new FormData(e.target));
    try { await api('POST', '/api/v1/admin/programs', f); location.hash = '#/admin/p/' + enc(f.slug); }
    catch (err) { document.getElementById('err').textContent = err.message; }
  });
}

function suggestNext(rels) {
  const v = rels.find((r) => !/-/.test(r.version))?.version || rels[0]?.version;
  if (!v) return '1.0.0';
  const m = v.match(/^(\d+)\.(\d+)\.(\d+)/); return m ? `${m[1]}.${m[2]}.${+m[3] + 1}` : '';
}

async function pageAdminProgram(slug) {
  const [p, rels] = await Promise.all([api('GET', `/api/v1/programs/${enc(slug)}`), api('GET', `/api/v1/admin/programs/${enc(slug)}/releases`)]);
  const base = `/api/v1/admin/programs/${enc(slug)}`;
  const platOpts = (sel) => PLATFORMS.map((x) => `<option${x === sel ? ' selected' : ''}>${x}</option>`).join('');
  $app.innerHTML = adminNav('') + `<div class="row spread"><h1>${esc(p.name)} <span class="muted" style="font-size:15px">${esc(p.slug)}</span></h1>
      <a class="btn" href="#/p/${enc(slug)}">공개 페이지 보기</a></div>
    <details class="card"><summary>프로그램 정보 수정</summary><form id="editprog">
      <label>이름</label><input type="text" name="name" value="${esc(p.name)}" required>
      <label>한 줄 소개</label><input type="text" name="summary" value="${esc(p.summary)}">
      <label>설명 (Markdown)</label><textarea name="description">${esc(p.description)}</textarea>
      <div class="err" id="err-prog"></div><div class="row spread"><button class="btn primary">저장</button>
      <button type="button" class="btn danger" data-act="del-prog">프로그램 삭제</button></div></form></details>

    <h2>새 버전 만들기</h2>
    <form class="card" id="newrel">
      <div class="row"><div style="flex:1"><label>버전 (SemVer: MAJOR.MINOR.PATCH, 예 1.4.0 / 2.0.0-beta.1)</label><input type="text" name="version" value="${esc(suggestNext(rels))}" required></div>
      <div><label>채널</label><select name="channel"><option>stable</option><option>beta</option></select></div></div>
      <label>패치 노트 (Markdown)</label><textarea name="notes" placeholder="### 추가&#10;- 새 기능&#10;### 수정&#10;- 버그 수정"></textarea>
      <label class="inline"><input type="checkbox" name="mandatory"> 필수 업데이트 (이 버전 이전 사용자는 반드시 업데이트)</label>
      <div class="err" id="err-rel"></div><button class="btn primary">초안 생성</button>
      <p class="muted">초안 생성 → 파일 업로드 → [배포] 순서로 진행합니다. 배포 후에는 파일을 바꿀 수 없으며, 수정이 필요하면 새 버전을 만드세요.</p></form>

    <h2>버전 목록</h2>
    ${rels.map((r) => `<div class="card" data-version="${esc(r.version)}">
      <div class="row spread"><div class="row"><h3>v${esc(r.version)}</h3><span class="badge ${r.status}">${statusKo[r.status]}</span>${badges(r)}</div>
        <div class="row">
          ${r.status === 'draft' ? `<button class="btn small primary" data-act="publish">배포</button><button class="btn small danger" data-act="del-rel">삭제</button>` : ''}
          ${r.status === 'published' ? `<button class="btn small" data-act="unpublish">초안으로</button><button class="btn small danger" data-act="yank">회수</button>` : ''}
          ${r.status === 'yanked' ? `<button class="btn small" data-act="publish">복원</button>` : ''}
        </div></div>
      <div class="muted">생성 ${fmtDate(r.created_at)} · 배포 ${fmtDate(r.published_at)}</div>
      ${assetTable(r, true)}
      ${r.status === 'draft' ? `<form class="upload" style="margin-top:10px"><div class="row">
          <select name="platform" style="width:auto">${platOpts('windows-amd64')}</select>
          <input type="file" name="file" multiple required><button class="btn">업로드</button></div>
          <div class="progress"><div></div></div><div class="err"></div></form>` : ''}
      <details style="margin-top:10px"><summary>패치 노트 / 설정 수정</summary><form class="editrel">
        <textarea name="notes">${esc(r.notes)}</textarea>
        <div class="row"><select name="channel" style="width:auto"><option${r.channel === 'stable' ? ' selected' : ''}>stable</option><option${r.channel === 'beta' ? ' selected' : ''}>beta</option></select>
        <label class="inline"><input type="checkbox" name="mandatory"${r.mandatory ? ' checked' : ''}> 필수 업데이트</label>
        <button class="btn small primary">저장</button></div><div class="err"></div></form></details>
      ${r.notes ? `<details><summary>미리보기</summary>${md(r.notes)}</details>` : ''}
    </div>`).join('') || '<p class="muted">아직 버전이 없습니다.</p>'}`;

  document.getElementById('editprog').addEventListener('submit', async (e) => {
    e.preventDefault();
    try { await api('PUT', base, Object.fromEntries(new FormData(e.target))); route(); }
    catch (err) { document.getElementById('err-prog').textContent = err.message; }
  });
  document.getElementById('newrel').addEventListener('submit', async (e) => {
    e.preventDefault(); const f = new FormData(e.target);
    try { await api('POST', base + '/releases', { version: f.get('version').trim(), channel: f.get('channel'), notes: f.get('notes'), mandatory: f.get('mandatory') === 'on' }); route(); }
    catch (err) { document.getElementById('err-rel').textContent = err.message; }
  });
  $app.querySelectorAll('form.editrel').forEach((form) => form.addEventListener('submit', async (e) => {
    e.preventDefault(); const v = form.closest('[data-version]').dataset.version; const f = new FormData(form);
    try { await api('PUT', `${base}/releases/${enc(v)}`, { notes: f.get('notes'), channel: f.get('channel'), mandatory: f.get('mandatory') === 'on' }); route(); }
    catch (err) { form.querySelector('.err').textContent = err.message; }
  }));
  $app.querySelectorAll('form.upload').forEach((form) => form.addEventListener('submit', (e) => {
    e.preventDefault(); const v = form.closest('[data-version]').dataset.version;
    const fd = new FormData(); fd.append('platform', form.platform.value);
    for (const file of form.file.files) fd.append('file', file);
    const bar = form.querySelector('.progress > div'); const err = form.querySelector('.err');
    const btn = form.querySelector('button'); btn.disabled = true;
    const xhr = new XMLHttpRequest(); // XHR for upload progress
    xhr.open('POST', `${base}/releases/${enc(v)}/assets`);
    xhr.setRequestHeader('X-Requested-With', 'xhr');
    xhr.upload.onprogress = (ev) => { if (ev.lengthComputable) bar.style.width = (ev.loaded / ev.total * 100) + '%'; };
    xhr.onload = () => { btn.disabled = false; if (xhr.status < 300) route(); else { try { err.textContent = JSON.parse(xhr.responseText).error; } catch { err.textContent = xhr.statusText; } } };
    xhr.onerror = () => { btn.disabled = false; err.textContent = '업로드 실패 (네트워크)'; };
    xhr.send(fd);
  }));
  $app.dataset.slug = slug;
}

async function pageTokens() {
  const toks = await api('GET', '/api/v1/admin/tokens');
  $app.innerHTML = adminNav('/tokens') + `<h1>API 토큰</h1>
    <p class="muted">빌드 서버(CI)나 <code>fsctl publish</code>에서 로그인 없이 업로드할 때 사용합니다. 토큰은 생성 시 한 번만 표시됩니다.</p>
    <table class="card"><tr><th>이름</th><th>생성</th><th>마지막 사용</th><th></th></tr>
    ${toks.map((t) => `<tr><td>${esc(t.name)}</td><td>${fmtDate(t.created_at)}</td><td>${fmtDate(t.last_used)}</td>
      <td><button class="btn small danger" data-act="del-token" data-id="${t.id}">폐기</button></td></tr>`).join('') || '<tr><td colspan=4 class="muted">없음</td></tr>'}</table>
    <form class="card" id="newtok"><label>토큰 이름</label><div class="row"><input type="text" name="name" placeholder="jenkins" required style="flex:1"><button class="btn primary">생성</button></div>
      <div id="secret"></div></form>`;
  document.getElementById('newtok').addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      const r = await api('POST', '/api/v1/admin/tokens', { name: new FormData(e.target).get('name') });
      document.getElementById('secret').innerHTML = `<div class="secret">⚠ 지금만 표시됩니다. 안전한 곳에 보관하세요:<br><code>${esc(r.secret)}</code></div>`;
    } catch (err) { document.getElementById('secret').innerHTML = `<div class="err">${esc(err.message)}</div>`; }
  });
}

// ---------------------------------------------------------------- actions (event delegation)
$app.addEventListener('click', async (e) => {
  const b = e.target.closest('[data-act]'); if (!b) return;
  const act = b.dataset.act; const slug = $app.dataset.slug;
  const v = b.closest('[data-version]')?.dataset.version;
  const rpath = `/api/v1/admin/programs/${enc(slug)}/releases/${enc(v || '')}`;
  const confirmMsg = {
    publish: `v${v} 를 배포할까요? 배포 후에는 파일을 변경할 수 없습니다.`,
    yank: `v${v} 를 회수할까요? 다운로드·자동 업데이트 대상에서 제외되고, 이 버전 사용자에게 업데이트가 강제됩니다.`,
    unpublish: `v${v} 를 초안으로 되돌릴까요?`, 'del-rel': `초안 v${v} 를 삭제할까요?`,
    'del-asset': '이 파일을 삭제할까요?', 'del-prog': '프로그램과 모든 버전·파일을 삭제합니다. 계속할까요?', 'del-token': '이 토큰을 폐기할까요?',
  }[act];
  if (confirmMsg && !confirm(confirmMsg)) return;
  try {
    switch (act) {
      case 'logout': await api('POST', '/api/v1/auth/logout'); me = null; location.hash = '#/'; return;
      case 'publish': case 'yank': case 'unpublish': await api('POST', `${rpath}/${act}`); break;
      case 'del-rel': await api('DELETE', rpath); break;
      case 'del-asset': await api('DELETE', `/api/v1/admin/assets/${b.dataset.id}`); break;
      case 'del-token': await api('DELETE', `/api/v1/admin/tokens/${b.dataset.id}`); break;
      case 'del-prog': await api('DELETE', `/api/v1/admin/programs/${enc(slug)}`); location.hash = '#/admin'; return;
    }
    route();
  } catch (err) { alert(err.message); }
});

// ---------------------------------------------------------------- router
async function route() {
  const h = decodeURIComponent(location.hash.slice(1) || '/');
  let m;
  try {
    if ((m = h.match(/^\/p\/([^/]+)$/))) return await pageProgram(m[1]);
    if (h.startsWith('/admin')) {
      if (!(await ensureLogin())) return pageLogin();
      if ((m = h.match(/^\/admin\/p\/([^/]+)$/))) return await pageAdminProgram(m[1]);
      if (h === '/admin/tokens') return await pageTokens();
      return await pageAdmin();
    }
    return await pageHome();
  } catch (err) {
    if (/login required/.test(err.message)) { me = null; return pageLogin(); }
    $app.innerHTML = `<div class="card err">오류: ${esc(err.message)}</div>`;
  }
}
window.addEventListener('hashchange', route);
route();
