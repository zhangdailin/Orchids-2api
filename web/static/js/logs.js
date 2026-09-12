// Log centre: three journals (request / operation / system) sharing one filter
// bar and one detail panel. Request details list every upstream attempt; change
// details show the redacted summary the server produced.
(function () {
  const state = { kind: 'request', cursor: '', records: [], selected: null };

  function el(id) {
    return document.getElementById(id);
  }

  function text(value, fallback) {
    if (value === undefined || value === null || value === '') return fallback || '—';
    return String(value);
  }

  function formatTime(value) {
    if (!value) return '—';
    const date = new Date(value);
    if (Number.isNaN(date.getTime())) return String(value);
    return date.toLocaleString();
  }

  function statusBadge(status) {
    const span = document.createElement('span');
    const normalized = String(status || '').toLowerCase();
    span.className = 'logs-badge';
    if (['success', 'ok', 'stop', '2xx', 'recovered'].includes(normalized)) span.classList.add('is-ok');
    else if (['error', 'firing', '4xx', '5xx'].includes(normalized)) span.classList.add('is-error');
    span.textContent = text(status, '—');
    return span;
  }

  function filters() {
    const params = new URLSearchParams();
    const map = {
      channel: 'filterChannel',
      model: 'filterModel',
      status: 'filterStatus',
      actor: 'filterActor',
      action: 'filterAction',
    };
    Object.keys(map).forEach((key) => {
      const node = el(map[key]);
      const value = node && node.value ? node.value.trim() : '';
      if (value) params.set(key, value);
    });
    return params;
  }

  function renderRows(append) {
    const body = el('logsRows');
    if (!body) return;
    if (!append) body.replaceChildren();
    if (state.records.length === 0 && !append) {
      const tr = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 5;
      td.className = 'ops-empty';
      td.textContent = '这条日志暂时没有记录。';
      tr.appendChild(td);
      body.appendChild(tr);
      return;
    }
    const start = append ? body.querySelectorAll('tr').length : 0;
    state.records.slice(start).forEach((record, index) => {
      const event = record.event || {};
      const tr = document.createElement('tr');
      tr.dataset.index = String(start + index);

      const time = document.createElement('td');
      time.textContent = formatTime(event.timestamp);
      tr.appendChild(time);

      const action = document.createElement('td');
      action.textContent = text(event.channel || event.kind, '—') + ' · ' + text(event.action, '—');
      if (event.model) {
        const badge = document.createElement('span');
        badge.className = 'logs-badge';
        badge.textContent = event.model;
        action.appendChild(badge);
      }
      tr.appendChild(action);

      const target = document.createElement('td');
      target.textContent = text(event.target || event.account_id || '', '—');
      tr.appendChild(target);

      const status = document.createElement('td');
      status.appendChild(statusBadge(event.status));
      tr.appendChild(status);

      const duration = document.createElement('td');
      duration.textContent = event.duration_ms ? event.duration_ms + ' ms' : '—';
      tr.appendChild(duration);

      tr.addEventListener('click', () => select(start + index));
      body.appendChild(tr);
    });
  }

  function row(label, value) {
    const dt = document.createElement('dt');
    dt.textContent = label;
    const dd = document.createElement('dd');
    dd.textContent = value;
    return [dt, dd];
  }

  function renderDetail(record) {
    const panel = el('logsDetail');
    if (!panel) return;
    panel.replaceChildren();
    const heading = document.createElement('h2');
    heading.textContent = '详情';
    panel.appendChild(heading);
    if (!record) {
      const empty = document.createElement('p');
      empty.className = 'ops-empty';
      empty.textContent = '选择左侧一条记录查看详情。';
      panel.appendChild(empty);
      return;
    }
    const event = record.event || {};
    const list = document.createElement('dl');
    const entries = [
      ['时间', formatTime(event.timestamp)],
      ['类型', text(event.kind, '—')],
      ['动作', text(event.action, '—')],
      ['渠道', text(event.channel, '—')],
      ['模型', text(event.model, '—')],
      ['账号', text(event.account_id, '—')],
      ['请求 ID', text(event.request_id, '—')],
      ['操作者', text(event.actor, '—')],
      ['来源 IP', text(event.client_ip, '—')],
      ['状态', text(event.status, '—')],
      ['总耗时', event.duration_ms ? event.duration_ms + ' ms' : '—'],
      ['首 Token', event.first_token_ms ? event.first_token_ms + ' ms' : event.first_token_ms === 0 ? '—' : '—'],
      ['Token', (event.input_tokens || 0) + ' in / ' + (event.output_tokens || 0) + ' out'],
    ];
    entries.forEach(([label, value]) => {
      const [dt, dd] = row(label, value);
      list.appendChild(dt);
      list.appendChild(dd);
    });
    panel.appendChild(list);

    if (event.error) {
      const errorTitle = document.createElement('div');
      errorTitle.textContent = '失败原因';
      panel.appendChild(errorTitle);
      const pre = document.createElement('pre');
      pre.textContent = event.error;
      panel.appendChild(pre);
    }

    if (event.details) {
      const detailTitle = document.createElement('div');
      detailTitle.textContent = '变更摘要（凭据已脱敏）';
      panel.appendChild(detailTitle);
      const pre = document.createElement('pre');
      pre.textContent = event.details;
      panel.appendChild(pre);
      if (event.redacted && event.redacted.length) {
        const maskNote = document.createElement('p');
        maskNote.className = 'ops-empty';
        maskNote.textContent = '已脱敏字段：' + event.redacted.join('、');
        panel.appendChild(maskNote);
      }
    }

    const attempts = record.attempts || [];
    if (attempts.length) {
      const attemptsTitle = document.createElement('div');
      attemptsTitle.textContent = `上游尝试（${attempts.length} 次）`;
      panel.appendChild(attemptsTitle);
      const ul = document.createElement('ul');
      ul.className = 'logs-attempts';
      attempts.forEach((attempt) => {
        const li = document.createElement('li');
        li.className = 'logs-attempt';
        const head = document.createElement('div');
        head.textContent = `第 ${text(attempt.attempt, '?')} 次 · ${text(attempt.provider, '—')} · ${attempt.duration_ms || 0} ms`;
        head.appendChild(statusBadge(attempt.status));
        li.appendChild(head);
        const meta = document.createElement('div');
        meta.className = 'ops-empty';
        const upstream = attempt.metadata && attempt.metadata.upstream_url ? attempt.metadata.upstream_url : '';
        const httpStatus = attempt.metadata && attempt.metadata.http_status ? attempt.metadata.http_status : '';
        meta.textContent = [upstream, httpStatus ? 'HTTP ' + httpStatus : ''].filter(Boolean).join(' · ');
        li.appendChild(meta);
        ul.appendChild(li);
      });
      panel.appendChild(ul);
    }
  }

  function select(index) {
    const record = state.records[index];
    if (!record) return;
    state.selected = index;
    document.querySelectorAll('#logsRows tr').forEach((tr) => {
      tr.classList.toggle('is-selected', Number(tr.dataset.index) === index);
    });
    renderDetail(record);
  }

  async function load(append) {
    const params = filters();
    params.set('kind', state.kind);
    params.set('limit', '50');
    if (append && state.cursor) params.set('before', state.cursor);
    try {
      const response = await fetch('/api/journal/records?' + params.toString(), { credentials: 'same-origin' });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      state.cursor = payload.next_cursor || '';
      state.records = append ? state.records.concat(payload.data || []) : payload.data || [];
      renderRows(append);
      const coverage = el('logsCoverage');
      if (coverage) {
        const info = payload.coverage || {};
        coverage.textContent = `保留 ${info.entries || 0} 条审计记录` +
          (info.oldest ? `，最早 ${info.oldest}` : '') +
          (info.newest ? `，最新 ${info.newest}` : '') +
          '；为空是“保留窗口内没有匹配”，不代表从未发生。';
      }
      const more = el('logsMore');
      if (more) more.hidden = !state.cursor;
      if (!append) renderDetail(null);
    } catch (error) {
      const body = el('logsRows');
      if (body) {
        body.replaceChildren();
        const tr = document.createElement('tr');
        const td = document.createElement('td');
        td.colSpan = 5;
        td.className = 'ops-empty';
        td.textContent = '读取日志失败：' + (error.message || error);
        tr.appendChild(td);
        body.appendChild(tr);
      }
    }
  }

  function bind() {
    document.querySelectorAll('.logs-tab').forEach((tab) => {
      tab.addEventListener('click', () => {
        document.querySelectorAll('.logs-tab').forEach((other) => {
          other.classList.remove('is-active');
          other.setAttribute('aria-selected', 'false');
        });
        tab.classList.add('is-active');
        tab.setAttribute('aria-selected', 'true');
        state.kind = tab.getAttribute('data-kind') || 'request';
        state.cursor = '';
        load(false);
      });
    });
    const form = el('logsFilters');
    if (form) form.addEventListener('submit', (event) => { event.preventDefault(); state.cursor = ''; load(false); });
    const clear = el('logsClear');
    if (clear) {
      clear.addEventListener('click', () => {
        ['filterChannel', 'filterModel', 'filterStatus', 'filterActor', 'filterAction'].forEach((id) => {
          const node = el(id);
          if (node) node.value = '';
        });
        state.cursor = '';
        load(false);
      });
    }
    const more = el('logsMore');
    if (more) more.addEventListener('click', () => load(true));
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => { bind(); load(false); });
  } else {
    bind();
    load(false);
  }
})();
