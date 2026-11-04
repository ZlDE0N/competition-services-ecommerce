/* Competition Services · E-commerce — UI behaviour.
   Everything dynamic is built with textContent / createElement (never innerHTML);
   the demo's HTML snippets pass through a small allow-list renderer. */
(function () {
  'use strict';

  var root = document.documentElement;
  root.classList.add('js');
  var reduce = window.matchMedia('(prefers-reduced-motion: reduce)').matches;
  var $ = function (sel, ctx) { return (ctx || document).querySelector(sel); };
  var $$ = function (sel, ctx) { return Array.prototype.slice.call((ctx || document).querySelectorAll(sel)); };

  function h(tag, cls, text) {
    var e = document.createElement(tag);
    if (cls) e.className = cls;
    if (text != null) e.textContent = text;
    return e;
  }
  function icon(name) {
    var ns = 'http://www.w3.org/2000/svg';
    var svg = document.createElementNS(ns, 'svg');
    svg.setAttribute('class', 'ic');
    var use = document.createElementNS(ns, 'use');
    use.setAttribute('href', '#i-' + name);
    svg.appendChild(use);
    return svg;
  }
  var wait = function (ms) { return new Promise(function (r) { setTimeout(r, reduce ? Math.min(ms, 40) : ms); }); };

  /* ── Theme ─────────────────────────────────────────────── */
  var KEY = 'cs-theme';
  function storedTheme() { try { return localStorage.getItem(KEY); } catch (e) { return null; } }
  function applyTheme(t) {
    root.setAttribute('data-theme', t);
    var b = $('#theme');
    if (b) {
      b.setAttribute('aria-pressed', String(t === 'light'));
      b.setAttribute('aria-label', t === 'light' ? 'Switch to dark theme' : 'Switch to light theme');
    }
  }
  applyTheme(storedTheme() || (window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'));
  $('#theme').addEventListener('click', function () {
    var next = root.getAttribute('data-theme') === 'light' ? 'dark' : 'light';
    applyTheme(next);
    try { localStorage.setItem(KEY, next); } catch (e) { /* storage may be blocked */ }
  });

  /* ── Nav state + scroll reveal ─────────────────────────── */
  var nav = $('#nav');
  function onScroll() { nav.classList.toggle('is-stuck', window.scrollY > 8); }
  onScroll();
  window.addEventListener('scroll', onScroll, { passive: true });

  $$('.reveal').forEach(function (el) {
    var siblings = $$('.reveal', el.parentElement).filter(function (s) { return s.parentElement === el.parentElement; });
    el.style.setProperty('--i', String(Math.max(0, siblings.indexOf(el))));
  });
  if ('IntersectionObserver' in window && !reduce) {
    var io = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (en.isIntersecting) { en.target.classList.add('is-in'); io.unobserve(en.target); }
      });
    }, { threshold: 0.12, rootMargin: '0px 0px -6% 0px' });
    $$('.reveal').forEach(function (el) { io.observe(el); });
  } else {
    $$('.reveal').forEach(function (el) { el.classList.add('is-in'); });
  }

  if ('IntersectionObserver' in window) {
    var links = $$('.nav__links a');
    var spy = new IntersectionObserver(function (entries) {
      entries.forEach(function (en) {
        if (!en.isIntersecting) return;
        links.forEach(function (a) { a.classList.toggle('is-on', a.getAttribute('href') === '#' + en.target.id); });
      });
    }, { rootMargin: '-45% 0px -50% 0px' });
    ['how', 'capabilities', 'demo', 'market', 'connect', 'security'].forEach(function (id) { var s = document.getElementById(id); if (s) spy.observe(s); });
  }

  /* ── Hero numbers ──────────────────────────────────────── */
  $$('.score[data-score] b').forEach(function (b) {
    var target = Number(b.parentElement.getAttribute('data-score'));
    if (reduce) { b.textContent = String(target); return; }
    var t0 = performance.now();
    (function tick(now) {
      var p = Math.min(1, (now - t0) / 1400);
      b.textContent = String(Math.round(target * (1 - Math.pow(1 - p, 3))));
      if (p < 1) requestAnimationFrame(tick);
    })(t0);
  });

  /* ── Safe rich text: only simple formatting tags survive ── */
  var ALLOWED = { P: 1, UL: 1, OL: 1, LI: 1, STRONG: 1, B: 1, EM: 1, I: 1, BR: 1 };
  function cloneSafe(node, into) {
    node.childNodes.forEach(function (n) {
      if (n.nodeType === 3) { into.appendChild(document.createTextNode(n.nodeValue)); return; }
      if (n.nodeType !== 1) return;
      if (ALLOWED[n.tagName]) {
        var copy = document.createElement(n.tagName.toLowerCase());
        cloneSafe(n, copy);
        into.appendChild(copy);
      } else if (n.tagName !== 'SCRIPT' && n.tagName !== 'STYLE') {
        cloneSafe(n, into);
      }
    });
  }
  function rich(el, html) {
    el.textContent = '';
    if (!html) { el.textContent = '(empty)'; el.classList.add('is-empty'); return; }
    el.classList.remove('is-empty');
    cloneSafe(new DOMParser().parseFromString(html, 'text/html').body, el);
  }

  /* ── Demo ──────────────────────────────────────────────── */
  var app = $('#demo-app');
  var S = { catalog: [], selected: null, status: 'pending', busy: false, lastApproved: null };

  function api(path, opts) {
    var init = opts || {};
    init.headers = { 'Content-Type': 'application/json' };
    return fetch(path, init).then(function (r) {
      return r.json().catch(function () { return {}; }).then(function (body) {
        if (!r.ok) throw new Error(body.error || r.statusText);
        return body;
      });
    });
  }
  function flash(msg) {
    var f = h('div', 'qempty', msg);
    f.setAttribute('role', 'alert');
    $('#stage-run').prepend(f);
    setTimeout(function () { f.remove(); }, 4500);
  }

  function renderCatalog() {
    var ul = $('#catalog');
    ul.textContent = '';
    S.catalog.forEach(function (p) {
      var li = h('li');
      var b = h('button', 'pitem');
      b.type = 'button';
      b.setAttribute('aria-pressed', String(p.id === S.selected));
      b.setAttribute('data-status', p.status);
      b.appendChild(h('span', 'pitem__n', p.name));
      b.appendChild(h('span', 'pitem__m', p.brand + ' · ' + p.category));
      var label = p.status === 'pending' ? 'Queued · ' + p.completeness + '%' : p.status === 'approved' ? 'Approved · ' + p.completeness + '%' : p.completeness + '%';
      b.appendChild(h('span', 'pitem__s', label));
      b.addEventListener('click', function () { selectProduct(p.id); });
      li.appendChild(b);
      ul.appendChild(li);
    });
  }
  function loadCatalog() {
    return api('/demo/catalog').then(function (d) { S.catalog = d.items; renderCatalog(); });
  }

  function selectProduct(id) {
    if (S.busy) return;
    S.selected = id;
    var p = S.catalog.filter(function (x) { return x.id === id; })[0];
    $('#stage-empty').hidden = true;
    $('#stage-run').hidden = false;
    $('#run-name').textContent = p.name;
    $('#run-meta').textContent = p.brand + ' · ' + p.category + ' · ' + p.sku;
    $('#timeline').textContent = '';
    $('#result').hidden = true;
    renderCatalog();
  }

  function buildTimeline(stages) {
    var ol = $('#timeline');
    ol.textContent = '';
    return stages.map(function (s) {
      var li = h('li', 'tl');
      li.setAttribute('data-state', 'pending');
      var dot = h('span', 'tl__i');
      dot.appendChild(icon('check'));
      li.appendChild(dot);
      li.appendChild(h('span', 'tl__l', s.label));
      li.appendChild(h('span', 'tl__n', s.note || ''));
      ol.appendChild(li);
      return li;
    });
  }

  function dl(target, rows) {
    target.textContent = '';
    rows.forEach(function (r) {
      target.appendChild(h('dt', null, r[0]));
      var dd = h('dd');
      if (r[2]) rich(dd, r[1]);
      else if (Array.isArray(r[1])) dd.textContent = r[1].length ? r[1].join(', ') : '(none)';
      else { dd.textContent = r[1] || '(empty)'; if (!r[1]) dd.classList.add('is-empty'); }
      target.appendChild(dd);
    });
  }
  function setRing(el, value) {
    el.querySelector('b').textContent = value + '%';
    requestAnimationFrame(function () { el.style.setProperty('--p', String(value)); });
  }

  function showResult(res) {
    var o = res.originalData, e = res.enhancedData;
    dl($('#diff-before'), [['Name', o.name], ['Short description', o.shortDescription], ['Long description', o.fullDescription], ['Tags', o.tags || []]]);
    dl($('#diff-after'), [
      ['Name', e.name], ['Short description', e.shortDescription, true], ['Long description', e.fullDescription, true],
      ['Tags', e.tags], ['Visual tags', e.visualTags], ['SEO title', e.tituloSEO], ['SEO description', e.descripcionSEO]
    ]);
    $('#res-conf').textContent = 'Confidence ' + Math.round(res.confidence * 100) + '%';
    $('#res-time').textContent = 'Pipeline ' + (res.processingTime / 1000).toFixed(1) + ' s (simulated)';
    $('#res-rule').textContent = 'Rule: ' + res.ruleApplied.name;
    $('#ring-before').style.setProperty('--p', '0');
    $('#ring-after').style.setProperty('--p', '0');
    $('#result').hidden = false;
    setRing($('#ring-before'), res.completeness.before);
    setTimeout(function () { setRing($('#ring-after'), res.completeness.after); }, reduce ? 0 : 450);
  }

  function runEnrich() {
    if (S.busy || !S.selected) return;
    S.busy = true;
    var btn = $('#run-btn');
    btn.disabled = true;
    $('#result').hidden = true;
    api('/demo/enrich', { method: 'POST', body: JSON.stringify({ productId: S.selected }) })
      .then(function (res) {
        var items = buildTimeline(res.stages);
        var chain = Promise.resolve();
        res.stages.forEach(function (s, i) {
          chain = chain.then(function () {
            if (!s.used) { items[i].setAttribute('data-state', 'skipped'); return wait(120); }
            items[i].setAttribute('data-state', 'active');
            return wait(Math.min(700, Math.max(220, s.ms * 0.45))).then(function () { items[i].setAttribute('data-state', 'done'); });
          });
        });
        return chain.then(function () { showResult(res); });
      })
      .then(function () { return Promise.all([loadCatalog(), loadQueue()]); })
      .catch(function (err) { flash('Could not run the demo: ' + err.message); })
      .then(function () { S.busy = false; btn.disabled = false; });
  }

  function queueItem(q) {
    var li = h('li', 'qitem');
    li.appendChild(h('h4', null, q.productName));
    li.appendChild(h('p', null, q.sku + ' · ' + q.id + (q.rejectionReason ? ' · ' + q.rejectionReason : '')));
    if (q.status === 'pending') {
      var row = h('div', 'qitem__a');
      var ok = h('button', 'btn btn--primary btn--sm', 'Approve'); ok.type = 'button';
      var no = h('button', 'btn btn--ghost btn--sm', 'Reject'); no.type = 'button';
      ok.addEventListener('click', function () { review(q.id, 'approve'); });
      no.addEventListener('click', function () { review(q.id, 'reject'); });
      row.appendChild(ok); row.appendChild(no);
      li.appendChild(row);
    }
    return li;
  }
  function loadQueue() {
    return api('/demo/queue?status=' + S.status).then(function (d) {
      var ul = $('#queue');
      ul.textContent = '';
      if (!d.items.length) {
        ul.appendChild(h('li', 'qempty', S.status === 'pending' ? 'Nothing waiting. Enrich a product to add one.' : 'No ' + S.status + ' items yet.'));
        return;
      }
      d.items.forEach(function (q) { ul.appendChild(queueItem(q)); });
    });
  }
  function showPayload() {
    return api('/demo/queue?status=approved').then(function (d) {
      var wrap = $('#payload-wrap');
      if (!d.items.length) { wrap.hidden = true; return; }
      $('#payload').textContent = JSON.stringify({ items: d.items.slice(0, 1), total: d.total }, null, 2);
      wrap.hidden = false;
      wrap.classList.add('is-in');
    });
  }
  function review(id, action) {
    api('/demo/queue/' + encodeURIComponent(id) + '/' + action, { method: 'POST', body: JSON.stringify({ reason: action === 'reject' ? 'Does not match the brand tone' : '' }) })
      .then(function () { return Promise.all([loadQueue(), loadCatalog(), showPayload()]); })
      .catch(function (err) { flash(err.message); });
  }

  if (app) {
    $('#run-btn').addEventListener('click', runEnrich);
    $('#reset').addEventListener('click', function () {
      api('/demo/reset', { method: 'POST', body: '{}' }).then(function () {
        S.selected = null;
        $('#stage-run').hidden = true; $('#stage-empty').hidden = false; $('#payload-wrap').hidden = true;
        return Promise.all([loadCatalog(), loadQueue()]);
      });
    });
    $$('#qtabs button').forEach(function (b) {
      b.addEventListener('click', function () {
        S.status = b.getAttribute('data-status');
        $$('#qtabs button').forEach(function (x) { x.setAttribute('aria-selected', String(x === b)); });
        loadQueue();
      });
    });
    Promise.all([loadCatalog(), loadQueue()]).catch(function () {
      $('#stage-empty').querySelector('p').textContent = 'The demo API is not reachable from here.';
    });
  }

  /* ── Market radar ──────────────────────────────────────── */
  var mkt = $('#market-app');
  if (mkt) {
    var M = { data: null, kind: 'all', sel: null };
    var NS = 'http://www.w3.org/2000/svg';
    var KIND = { marketplace: 'Marketplace', 'cross-border': 'Cross-border', retailer: 'Retailer', platform: 'Platform' };
    var QLABEL = { high: 'solid data', medium: 'indicative', low: 'weak data' };

    function safeLink(url, label) {
      var a = h('a', null, label);
      try {
        var u = new URL(url);
        if (u.protocol === 'https:') { a.href = u.href; a.target = '_blank'; a.rel = 'noopener noreferrer'; }
      } catch (e) { /* not a URL: render as plain text */ }
      return a;
    }
    function hostOf(url) { try { return new URL(url).hostname.replace(/^www\./, ''); } catch (e) { return 'source'; } }

    function renderTotals(d) {
      $('#mkt-caveat').textContent = d.caveat + ' Snapshot: ' + d.asOf + '.';
      var dl = $('#mkt-totals');
      dl.textContent = '';
      d.totals.forEach(function (m) {
        var box = h('div');
        box.appendChild(h('dt', null, m.label.indexOf(m.period) >= 0 ? m.label : m.label + ' · ' + m.period));
        var dd = h('dd', null, m.value);
        var small = h('small');
        small.appendChild(safeLink(m.source, hostOf(m.source)));
        small.appendChild(document.createTextNode(' · ' + (QLABEL[m.quality] || m.quality)));
        dd.appendChild(small);
        box.appendChild(dd);
        dl.appendChild(box);
      });
    }

    function place(i, n) {
      var a = (-90 + (360 / n) * i) * Math.PI / 180;
      return { x: 50 + 38 * Math.cos(a), y: 50 + 36 * Math.sin(a) };
    }

    function renderNet() {
      var net = $('#net'), lines = $('#net-lines');
      $$('.node', net).forEach(function (n) { n.remove(); });
      lines.textContent = '';
      var ps = M.data.players;
      ps.forEach(function (p, i) {
        var pos = place(i, ps.length);
        var dim = M.kind !== 'all' && p.kind !== M.kind;
        var ln = document.createElementNS(NS, 'line');
        ln.setAttribute('x1', '50'); ln.setAttribute('y1', '50');
        ln.setAttribute('x2', pos.x.toFixed(1)); ln.setAttribute('y2', pos.y.toFixed(1));
        ln.setAttribute('data-threat', p.threat);
        ln.setAttribute('data-id', p.id);
        if (dim) ln.setAttribute('data-dim', '');
        lines.appendChild(ln);

        var b = h('button', 'node');
        b.type = 'button';
        b.style.left = pos.x.toFixed(1) + '%';
        b.style.top = pos.y.toFixed(1) + '%';
        b.setAttribute('aria-pressed', String(M.sel === p.id));
        b.setAttribute('data-id', p.id);
        if (dim) b.setAttribute('data-dim', '');
        var t = h('b');
        var dot = h('i', 'dot dot--' + p.threat);
        dot.setAttribute('aria-hidden', 'true');
        t.appendChild(dot);
        t.appendChild(document.createTextNode(p.name));
        b.appendChild(t);
        b.appendChild(h('small', null, (KIND[p.kind] || p.kind) + ' · ' + p.threat + ' threat'));
        b.addEventListener('click', function () { M.sel = p.id; renderNet(); renderDetail(); });
        net.appendChild(b);
      });
      var sel = $('line[data-id="' + (M.sel || '') + '"]', lines);
      if (sel) sel.setAttribute('data-on', '');
    }

    function renderDetail() {
      var box = $('#mkt-detail');
      var p = M.data.players.filter(function (x) { return x.id === M.sel; })[0];
      box.textContent = '';
      if (!p) { box.appendChild(h('p', 'muted', 'Select a node to see the evidence.')); return; }
      var title = h('h3', null, p.name);
      var tag = h('span', 'tag-threat', p.threat + ' threat');
      tag.setAttribute('data-threat', p.threat);
      title.appendChild(tag);
      box.appendChild(title);
      box.appendChild(h('p', null, p.summary));
      box.appendChild(h('h4', null, 'Evidence'));
      var ul = h('ul', 'evid');
      p.metrics.forEach(function (m) {
        var li = h('li');
        var left = h('span');
        left.appendChild(document.createTextNode(m.label + ': '));
        left.appendChild(h('b', null, m.value));
        li.appendChild(left);
        var q = h('span', 'q', QLABEL[m.quality] || m.quality);
        q.setAttribute('data-q', m.quality);
        li.appendChild(q);
        var small = h('small');
        small.appendChild(document.createTextNode(m.period + ' · '));
        small.appendChild(safeLink(m.source, hostOf(m.source)));
        if (m.note) small.appendChild(document.createTextNode(' · ' + m.note.replace(/https?:\/\/\S+/g, '').trim()));
        li.appendChild(small);
        ul.appendChild(li);
      });
      box.appendChild(ul);
      box.appendChild(h('h4', null, 'Suggested response'));
      box.appendChild(h('p', 'mkt__play', p.playbook));
    }

    $$('#mkt-filter button').forEach(function (b) {
      b.addEventListener('click', function () {
        M.kind = b.getAttribute('data-kind');
        $$('#mkt-filter button').forEach(function (x) { x.setAttribute('aria-pressed', String(x === b)); });
        if (M.sel) {
          var cur = M.data.players.filter(function (x) { return x.id === M.sel; })[0];
          if (cur && M.kind !== 'all' && cur.kind !== M.kind) { M.sel = null; renderDetail(); }
        }
        renderNet();
      });
    });

    api('/demo/market?country=ar').then(function (d) {
      M.data = d;
      renderTotals(d);
      renderNet();
    }).catch(function () {
      $('#mkt-caveat').textContent = 'The market snapshot is not reachable from here.';
    });
  }

  /* ── Code tabs + copy ──────────────────────────────────── */
  $$('#ctabs button').forEach(function (b) {
    b.addEventListener('click', function () {
      var tab = b.getAttribute('data-tab');
      $$('#ctabs button').forEach(function (x) { x.setAttribute('aria-selected', String(x === b)); });
      $$('.code__body').forEach(function (p) { p.hidden = p.getAttribute('data-panel') !== tab; });
    });
  });
  $$('.copy').forEach(function (btn) {
    btn.addEventListener('click', function () {
      var code = document.getElementById('code-' + btn.getAttribute('data-copy'));
      var label = btn.querySelector('span');
      function done() { label.textContent = 'Copied'; setTimeout(function () { label.textContent = 'Copy'; }, 1600); }
      if (navigator.clipboard && navigator.clipboard.writeText) navigator.clipboard.writeText(code.textContent).then(done, function () { label.textContent = 'Select and copy'; });
      else label.textContent = 'Select and copy';
    });
  });
})();
