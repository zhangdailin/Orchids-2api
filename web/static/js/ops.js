// Operations overview: KPI cards, a per-minute trend and the channel × model
// matrix. Every figure carries its sample count so "no traffic" is rendered as
// 暂无样本 instead of a healthy-looking zero.
(function () {
  const state = { window: 180, channel: '', timer: null };

  function el(id) {
    return document.getElementById(id);
  }

  function formatRate(rate, samples) {
    if (!samples) return '暂无样本';
    return (rate * 100).toFixed(1) + '%';
  }

  // 45279 ms is unreadable at a glance; 45.3 s is. The raw millisecond value is
  // never dropped: it is repeated in the note / title so a slow regression can
  // still be read off the card.
  function formatDuration(value, samples) {
    if (!samples || !value) return '暂无样本';
    const ms = Number(value);
    if (!Number.isFinite(ms) || ms <= 0) return '暂无样本';
    if (ms < 1000) return Math.round(ms) + ' ms';
    if (ms < 60000) return (ms / 1000).toFixed(1) + ' s';
    return (ms / 60000).toFixed(1) + ' min';
  }

  // Kept as the single formatting entry point so the KPI cards and the
  // channel × model matrix can never drift apart in unit or wording.
  function formatMs(value, samples) {
    return formatDuration(value, samples);
  }

  function preciseMs(value) {
    const ms = Number(value);
    if (!Number.isFinite(ms)) return '';
    return Math.round(ms) + ' ms';
  }

  // "P95" is the whole point of these two cards: the number is the 95th
  // percentile of the window, not an average, so say so on the card.
  function p95Note(samples, value, what) {
    if (!samples || !value) return `P95 分位 · ${what} · 暂无样本`;
    return `P95（非均值）· ${samples} 样本 · 最慢 5% · 精确 ${preciseMs(value)}`;
  }

  function p95Title(samples, value, what) {
    if (!samples || !value) return `P95 分位 · ${what} · 暂无样本`;
    return `${what}的 P95 分位（不是平均值）：${preciseMs(value)}，共 ${samples} 个样本，代表其中最慢的 5%。`;
  }

  // options: legacy boolean (muted) or { muted, belowTarget, title }.
  function kpi(label, value, note, options) {
    const opts = typeof options === 'boolean' ? { muted: options } : (options || {});
    const card = document.createElement('div');
    card.className = 'ops-kpi' + (opts.muted ? ' is-muted' : '') + (opts.belowTarget ? ' is-below-target' : '');
    if (opts.title) card.title = opts.title;
    const labelNode = document.createElement('div');
    labelNode.className = 'label';
    labelNode.textContent = label;
    const valueNode = document.createElement('div');
    valueNode.className = 'value';
    valueNode.textContent = value;
    card.appendChild(labelNode);
    card.appendChild(valueNode);
    if (note) {
      const noteNode = document.createElement('div');
      noteNode.className = 'note';
      noteNode.textContent = note;
      card.appendChild(noteNode);
    }
    return card;
  }

  // success_target / success_target_source are optional payload fields supplied
  // by the alerting configuration. When they are absent the card simply keeps
  // its plain "N 次失败" note — never "目标 undefined%".
  function targetNote(target, source, rate) {
    const value = Number(target);
    if (!Number.isFinite(value) || value <= 0 || value > 1) return '';
    const percent = (value * 100).toFixed(1);
    const origin = source ? `（${source}）` : '';
    const current = Number(rate);
    if (!Number.isFinite(current)) return `目标 ${percent}%${origin}`;
    const diff = (current - value) * 100;
    if (Math.abs(diff) < 0.05) return `目标 ${percent}%${origin} · 已达标`;
    const gap = Math.abs(diff).toFixed(1);
    return `目标 ${percent}%${origin} · 当前${diff < 0 ? '低' : '高'} ${gap} 个百分点`;
  }

  function successRateCard(totals, real, payload) {
    const rate = totals.success_rate || 0;
    const parts = [`${totals.failed || 0} 次失败`];
    const target = targetNote(payload && payload.success_target, payload && payload.success_target_source, rate);
    if (target) parts.push(target);
    const targetValue = Number(payload && payload.success_target);
    const below = Number.isFinite(targetValue) && targetValue > 0 && real > 0 && rate < targetValue;
    return kpi('成功率', formatRate(rate, real), parts.join(' · '), {
      muted: real === 0,
      belowTarget: below,
      title: below
        ? `窗口内成功率 ${formatRate(rate, real)}，低于告警阈值 ${(targetValue * 100).toFixed(1)}%。`
        : `窗口内成功率 ${formatRate(rate, real)}（成功 ${totals.success || 0} / 真实请求 ${real}）。`,
    });
  }

  function renderKpis(payload) {
    const container = el('opsKpis');
    if (!container) return;
    container.replaceChildren();
    const totals = payload.totals || {};
    const real = (totals.requests || 0) - (totals.probes || 0);
    const samples = totals.samples || 0;

    if (payload.available === false) {
      container.appendChild(kpi('指标聚合', '未启用', payload.note || '需要 Redis', true));
      return;
    }
    // 请求量 is a window total, RPM is the per-minute average over the very
    // same window. Spelling both out is what stops them looking contradictory.
    container.appendChild(kpi('请求量', String(Math.max(real, 0)), `${payload.window_minutes} 分钟窗口内累计 · 不含探测`, {
      title: `所选 ${payload.window_minutes} 分钟窗口内的真实请求累计值（已扣除 ${totals.probes || 0} 次合成探测）。`,
    }));
    container.appendChild(successRateCard(totals, real, payload));
    container.appendChild(kpi('RPM', real ? (totals.rpm || 0).toFixed(2) : '暂无样本', '窗口内平均 · 每分钟真实请求', {
      muted: real === 0,
      title: `同一窗口的平均值：${Math.max(real, 0)} 次 ÷ ${payload.window_minutes} 分钟，不是瞬时速率。`,
    }));
    // The two P95 cards: say what the number measures, that it is a percentile
    // and not an average, and keep the exact millisecond value around.
    container.appendChild(kpi('首 Token P95（响应前）', formatMs(totals.first_token_p95_ms, samples), p95Note(samples, totals.first_token_p95_ms, '响应前耗时'), {
      muted: samples === 0,
      title: p95Title(samples, totals.first_token_p95_ms, '响应前耗时'),
    }));
    container.appendChild(kpi('总耗时 P95（整段生成）', formatMs(totals.duration_p95_ms, samples), p95Note(samples, totals.duration_p95_ms, '整段生成耗时'), {
      muted: samples === 0,
      title: p95Title(samples, totals.duration_p95_ms, '整段生成耗时'),
    }));
    const concurrency = (payload.concurrency && payload.concurrency.accounts_refreshing) || 0;
    container.appendChild(kpi('刷新中账号', String(concurrency), '当前并发刷新'));
    container.appendChild(kpi('探测量', String(totals.probes || 0), '合成流量，不计入成功率'));
  }

  function renderTrend(series) {
    const container = el('opsTrend');
    if (!container) return;
    container.replaceChildren();
    if (!series || series.length === 0) {
      const empty = document.createElement('p');
      empty.className = 'ops-empty';
      empty.textContent = '这段时间没有流量样本。';
      container.appendChild(empty);
      return;
    }
    const peak = series.reduce((max, point) => Math.max(max, point.requests || 0), 1);
    series.slice(-240).forEach((point) => {
      const bar = document.createElement('div');
      const failed = (point.failed || 0) > 0;
      bar.className = 'bar' + (failed ? ' is-failed' : '');
      const height = Math.max(2, Math.round(((point.requests || 0) / peak) * 200));
      bar.style.height = height + 'px';
      bar.title = `${point.minute}\n请求 ${point.requests}（成功 ${point.success}，失败 ${point.failed}）`;
      container.appendChild(bar);
    });
  }

  function renderAlerts(alerts) {
    const list = el('opsAlerts');
    const counter = el('opsAlertCount');
    if (!list) return;
    list.replaceChildren();
    if (counter) counter.textContent = alerts.length ? `${alerts.length} 条告警中` : '当前无告警';
    if (!alerts.length) {
      const item = document.createElement('li');
      item.className = 'ops-empty';
      item.textContent = '当前没有触发告警。';
      list.appendChild(item);
      return;
    }
    alerts.forEach((alert) => {
      const item = document.createElement('li');
      item.className = 'ops-alert severity-' + (alert.severity || 'info');
      const title = document.createElement('div');
      title.className = 'title';
      title.textContent = alert.title || alert.key;
      const detail = document.createElement('div');
      detail.className = 'detail';
      detail.textContent = alert.detail || '';
      item.appendChild(title);
      item.appendChild(detail);
      list.appendChild(item);
    });
  }

  function historyCells(series) {
    const cell = document.createElement('td');
    cell.className = 'ops-history';
    const buckets = (series || []).slice(-40);
    if (!buckets.length) {
      cell.textContent = '—';
      return cell;
    }
    buckets.forEach((point) => {
      const bar = document.createElement('span');
      bar.className = 'ops-bar';
      const errorRate = point.requests ? (point.failed || 0) / point.requests : 0;
      if (errorRate > 0.5) bar.classList.add('is-bad');
      else if (errorRate > 0) bar.classList.add('is-warn');
      // 40 buckets at 4px + 2px used to push the row past a 736px window.
      bar.style.width = '3px';
      bar.style.marginRight = '1px';
      bar.title = `${point.minute}: ${point.requests} 请求 / ${point.failed} 失败`;
      cell.appendChild(bar);
    });
    return cell;
  }

  function matrixRow(label, row, options) {
    const tr = document.createElement('tr');
    tr.className = options.isModel ? 'is-model' : 'is-channel';

    const name = document.createElement('td');
    name.textContent = label;
    tr.appendChild(name);

    const accounts = document.createElement('td');
    if (options.isModel) accounts.textContent = '—';
    else if (row.accounts_enabled === 0) accounts.textContent = '未配置';
    else accounts.textContent = `${row.accounts_available} / ${row.accounts_enabled}` + (row.accounts_needing_login ? `（需登录 ${row.accounts_needing_login}）` : '');
    tr.appendChild(accounts);

    const requests = document.createElement('td');
    requests.textContent = options.isModel ? String(row.requests || 0) : String(Math.max((row.summary && row.summary.requests || 0) - (row.summary && row.summary.probes || 0), 0));
    tr.appendChild(requests);

    const rate = document.createElement('td');
    const samples = options.isModel ? row.samples || 0 : (row.summary && row.summary.samples) || 0;
    const r = options.isModel ? row.success_rate || 0 : (row.summary && row.summary.success_rate) || 0;
    rate.textContent = formatRate(r, samples);
    if (!samples) rate.className = 'ops-empty';
    tr.appendChild(rate);

    const ttftValue = options.isModel ? row.first_token_p95_ms : (row.summary && row.summary.first_token_p95_ms);
    const ttft = document.createElement('td');
    ttft.className = 'ops-duration';
    // Same formatter as the KPI cards: the two sets of P95 numbers can only be
    // compared if they are rendered with one rule.
    ttft.textContent = formatMs(ttftValue, samples);
    ttft.title = samples ? p95Title(samples, ttftValue, '响应前耗时') : '暂无样本';
    tr.appendChild(ttft);

    const durationValue = options.isModel ? row.duration_p95_ms : (row.summary && row.summary.duration_p95_ms);
    const duration = document.createElement('td');
    duration.className = 'ops-duration';
    duration.textContent = formatMs(durationValue, samples);
    duration.title = samples ? p95Title(samples, durationValue, '整段生成耗时') : '暂无样本';
    tr.appendChild(duration);

    const throttled = document.createElement('td');
    throttled.textContent = options.isModel ? '—' : String(row.model_cooldowns || 0);
    tr.appendChild(throttled);

    tr.appendChild(historyCells(options.isModel ? null : row.series));
    return tr;
  }

  function renderMatrix(rows) {
    const table = el('opsMatrix');
    if (!table) return;
    const body = table.querySelector('tbody');
    body.replaceChildren();
    if (!rows || rows.length === 0) {
      const tr = document.createElement('tr');
      const td = document.createElement('td');
      td.colSpan = 8;
      td.className = 'ops-empty';
      td.textContent = '还没有渠道数据。添加账号或等待流量后这里会出现状态矩阵。';
      tr.appendChild(td);
      body.appendChild(tr);
      return;
    }
    rows.forEach((row) => {
      body.appendChild(matrixRow(row.channel, row, { isModel: false }));
      (row.models || []).forEach((model) => {
        body.appendChild(matrixRow(model.model, model, { isModel: true }));
      });
    });
  }

  function renderCoverage(payload) {
    const node = el('opsCoverage');
    const label = el('opsWindowLabel');
    if (label) {
      const since = payload.since || '';
      const until = payload.until || '';
      const window = payload.window_minutes ? `窗口 ${payload.window_minutes} 分钟` : '窗口读取中';
      const retention = payload.retention_hours ? `；保留 ${payload.retention_hours} 小时` : '';
      label.textContent = [window, since && until ? `（${since} → ${until}）` : '', retention].join('') + '。';
    }
    if (!node) return;

    // The card must never be blank: a blank card is indistinguishable from a
    // broken page. Every path below states what is known and what is missing.
    const parts = [];
    const coverage = payload.coverage || {};
    if (typeof coverage.entries === 'number') {
      parts.push(`审计日志保留 ${coverage.entries} 条`);
      if (coverage.oldest) parts.push(`最早 ${coverage.oldest}`);
      if (coverage.newest) parts.push(`最新 ${coverage.newest}`);
    } else {
      parts.push('审计日志覆盖范围未能读取（接口未返回 coverage）');
    }
    if (coverage.counts) {
      const counts = Object.keys(coverage.counts).map((key) => `${key}=${coverage.counts[key]}`).join('、');
      if (counts) parts.push(`采样计数：${counts}`);
    }
    parts.push('因此页面只承诺“保留窗口内”的结论，不承诺固定天数。');

    // Say what is counted but deliberately not shown as a channel, instead of
    // letting http / probe look like missing or broken channels.
    const excluded = payload.excluded_aggregates || [];
    if (excluded.length) {
      const labels = excluded.map((name) => {
        if (name === 'http') return 'http（非推理路径：管理页、健康检查、公网扫描）';
        if (name === 'probe') return 'probe（本系统主动探测的合成流量）';
        return name;
      });
      parts.push('已计数但不在渠道矩阵中显示：' + labels.join('；'));
    }
    node.textContent = parts.join('；');
  }

  function updateChannelOptions(channels, current) {
    const select = el('opsChannel');
    if (!select) return;
    const options = ['<option value="">全部渠道</option>'].concat(
      (channels || []).map((channel) => `<option value="${channel}">${channel}</option>`)
    );
    select.innerHTML = options.join('');
    select.value = current || '';
  }

  async function load() {
    const params = new URLSearchParams({ window: String(state.window) });
    if (state.channel) params.set('channel', state.channel);
    try {
      const response = await fetch('/api/ops/overview?' + params.toString(), { credentials: 'same-origin' });
      if (!response.ok) throw new Error('HTTP ' + response.status);
      const payload = await response.json();
      renderKpis(payload);
      renderTrend(payload.series);
      renderAlerts(payload.alerts || []);
      renderMatrix(payload.matrix);
      // Coverage is rendered on every successful response, including the
      // "aggregation disabled" one, so the card always says what the numbers rest
      // on instead of staying at its placeholder.
      renderCoverage(payload);
      updateChannelOptions(payload.channels, state.channel);
      if (payload.available === false && payload.note) {
        const coverage = el('opsCoverage');
        if (coverage) coverage.textContent = payload.note;
      }
    } catch (error) {
      const container = el('opsKpis');
      if (container) {
        container.replaceChildren(kpi('读取失败', '—', String(error.message || error), true));
      }
      // An empty card is indistinguishable from a broken page, and the most
      // common cause here is an expired session.
      renderCoverage({ window_minutes: state.window, coverage: {}, excluded_aggregates: [] });
      const coverage = el('opsCoverage');
      if (coverage) {
        coverage.textContent = `指标读取失败：${String(error.message || error)}。会话可能已过期，请重新登录后刷新。`;
      }
    }
  }

  function bind() {
    document.querySelectorAll('.ops-chip[data-window]').forEach((button) => {
      button.addEventListener('click', () => {
        document.querySelectorAll('.ops-chip[data-window]').forEach((other) => other.classList.remove('is-active'));
        button.classList.add('is-active');
        state.window = Number(button.getAttribute('data-window')) || 180;
        load();
      });
    });
    const select = el('opsChannel');
    if (select) select.addEventListener('change', () => { state.channel = select.value; load(); });
    const refresh = el('opsRefresh');
    if (refresh) refresh.addEventListener('click', load);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', () => { bind(); load(); });
  } else {
    bind();
    load();
  }
  state.timer = setInterval(load, 60000);
})();
