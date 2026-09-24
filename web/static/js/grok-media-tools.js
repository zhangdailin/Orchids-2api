(() => {
  const CUSTOM_VOICE = '__custom__';
  const AUDIO_MIME = { mp3: 'audio/mpeg', wav: 'audio/wav', wave: 'audio/wav', opus: 'audio/ogg', ogg: 'audio/ogg', aac: 'audio/aac', flac: 'audio/flac', pcm: 'audio/L16' };

  function capability(route, name) {
    return Array.isArray(route?.capabilities) && route.capabilities.includes(name);
  }

  function normalizeVoices(payload) {
    const source = Array.isArray(payload) ? payload : (Array.isArray(payload?.voices) ? payload.voices : (Array.isArray(payload?.data) ? payload.data : []));
    return source.map((voice) => {
      if (typeof voice === 'string') return { id: voice, name: voice, language: '' };
      const id = String(voice?.voice_id || voice?.id || voice?.value || '').trim();
      return { id, name: String(voice?.name || voice?.display_name || id).trim(), language: String(voice?.language || voice?.locale || '').trim() };
    }).filter((voice) => voice.id);
  }

  function decodeBase64(value, mime) {
    const raw = String(value || '').replace(/^data:[^;,]+;base64,/, '').replace(/\s+/g, '');
    const bytes = Uint8Array.from(atob(raw), (character) => character.charCodeAt(0));
    return new Blob([bytes], { type: mime || 'audio/mpeg' });
  }

  function jsonAudioSource(payload, contentType) {
    const item = Array.isArray(payload?.data) ? payload.data[0] : null;
    const directURL = payload?.url || payload?.audio_url || item?.url;
    if (typeof directURL === 'string' && directURL.trim()) return { url: directURL.trim(), revoke: false };
    const encoded = payload?.audio || payload?.audio_base64 || payload?.b64_json || item?.audio || item?.audio_base64 || item?.b64_json;
    if (typeof encoded !== 'string' || !encoded.trim()) throw new Error('TTS JSON 响应中没有音频数据');
    const format = String(payload?.format || payload?.response_format || item?.format || '').toLowerCase();
    const mime = /^audio\//i.test(contentType || '') ? contentType.split(';')[0] : (AUDIO_MIME[format] || 'audio/mpeg');
    return { blob: decodeBase64(encoded, mime), revoke: true };
  }

  async function responseError(response) {
    const text = await response.text();
    try {
      const data = JSON.parse(text);
      return data?.error?.message || data?.message || text || `请求失败 (${response.status})`;
    } catch (_) {
      return text || `请求失败 (${response.status})`;
    }
  }

  function formatSeconds(value) {
    const number = Number(value);
    return Number.isFinite(number) ? `${number.toFixed(number < 10 ? 2 : 1)} 秒` : '';
  }

  function init() {
    const host = document.getElementById('voiceFileTools');
    if (!host || host.dataset.initialized === 'true') return;
    host.dataset.initialized = 'true';
    const el = (id) => document.getElementById(id);
    let routes = [], objectURL = '', busy = false, voiceRequest = 0;

    function setStatus(message, type) {
      const status = el('audioStatus');
      status.textContent = message || '';
      status.classList.toggle('is-error', type === 'error');
    }

    function clearObjectURL() {
      if (objectURL) URL.revokeObjectURL(objectURL);
      objectURL = '';
    }

    function selectedVoice() {
      return el('audioVoice').value === CUSTOM_VOICE ? el('audioCustomVoice').value.trim() : el('audioVoice').value;
    }

    function syncCustomVoice() {
      el('audioCustomVoiceBlock').classList.toggle('hidden', el('audioVoice').value !== CUSTOM_VOICE);
    }

    async function loadVoices() {
      const requestID = ++voiceRequest;
      const model = el('audioModel').value;
      const select = el('audioVoice');
      select.replaceChildren(new Option('加载音色…', ''));
      select.disabled = true;
      syncCustomVoice();
      if (!model || el('audioOperation').value !== 'tts') return;
      let payload, lastError;
      const prefix = window.GrokToolRequest?.prefix?.() || '/grok/v1';
      for (const endpoint of [`${prefix}/tts/voices`, '/v1/tts/voices']) {
        try {
          const response = await fetch(`${endpoint}?model=${encodeURIComponent(model)}`, { headers: window.GrokToolRequest?.headers?.({ Accept: 'application/json' }) || { Accept: 'application/json' } });
          if (!response.ok) throw new Error(await responseError(response));
          payload = await response.json();
          break;
        } catch (error) { lastError = error; }
      }
      if (requestID !== voiceRequest) return;
      const voices = normalizeVoices(payload);
      select.replaceChildren();
      for (const voice of voices) {
        const option = new Option(`${voice.name}${voice.language ? ` · ${voice.language}` : ''}`, voice.id);
        select.appendChild(option);
      }
      select.appendChild(new Option('自定义 voice_id…', CUSTOM_VOICE));
      select.disabled = false;
      if (!voices.length) {
        select.value = CUSTOM_VOICE;
        setStatus(lastError ? `音色列表不可用：${lastError.message}` : '未返回预设音色，请输入 voice_id', lastError ? 'error' : '');
      } else if (el('voiceName')?.value && voices.some((voice) => voice.id === el('voiceName').value)) {
        select.value = el('voiceName').value;
      }
      syncCustomVoice();
    }

    function refresh() {
      const tts = el('audioOperation').value === 'tts';
      el('audioTextLabel').hidden = !tts;
      el('audioFileLabel').hidden = tts;
      el('audioVoiceBlock').hidden = !tts;
      el('audioCustomVoiceBlock').hidden = !tts || el('audioVoice').value !== CUSTOM_VOICE;
      const select = el('audioModel');
      const previous = select.value;
      select.replaceChildren();
      for (const route of routes.filter((route) => capability(route, tts ? 'tts' : 'stt'))) {
        select.appendChild(new Option(route.id, route.id));
      }
      if (Array.from(select.options).some((option) => option.value === previous)) select.value = previous;
      el('audioSubmit').disabled = busy || !select.value;
      if (!select.value) setStatus('没有配置支持此操作的模型'); else setStatus('');
      if (tts) loadVoices(); else ++voiceRequest;
    }

    function acceptRoutes(value) {
      routes = Array.isArray(value) ? value : [];
      refresh();
      const select = el('imagineModel');
      if (!select) return;
      const previous = select.value;
      while (select.options.length > 1) select.remove(1);
      for (const route of routes.filter((route) => capability(route, 'image'))) select.appendChild(new Option(route.id, route.id));
      if (Array.from(select.options).some((option) => option.value === previous)) select.value = previous;
    }

    function renderTTS(source, response) {
      const result = el('audioResult');
      const audio = document.createElement('audio');
      audio.controls = true;
      audio.src = source.url;
      const link = document.createElement('a');
      link.className = 'btn btn-outline';
      link.href = source.url;
      const type = response.headers.get('Content-Type') || source.blob?.type || 'audio/mpeg';
      const extension = Object.entries(AUDIO_MIME).find(([, mime]) => type.startsWith(mime))?.[0] || 'mp3';
      link.download = `speech.${extension === 'wave' ? 'wav' : extension}`;
      link.textContent = '下载语音';
      result.append(audio, link);
    }

    function renderSTT(data) {
      const result = el('audioResult');
      const text = document.createElement('pre');
      text.className = 'voice-transcript';
      text.textContent = String(data?.text || '');
      result.appendChild(text);
      const metadata = document.createElement('div');
      metadata.className = 'voice-transcript-meta';
      if (data?.language) metadata.appendChild(Object.assign(document.createElement('span'), { textContent: `语言：${data.language}` }));
      if (data?.duration != null) metadata.appendChild(Object.assign(document.createElement('span'), { textContent: `时长：${formatSeconds(data.duration)}` }));
      if (metadata.children.length) result.appendChild(metadata);
      const words = Array.isArray(data?.words) ? data.words : [];
      if (!words.length) return;
      const table = document.createElement('table');
      table.className = 'voice-word-table';
      table.innerHTML = '<thead><tr><th>词语</th><th>开始</th><th>结束</th><th>说话人</th></tr></thead>';
      const body = document.createElement('tbody');
      for (const word of words) {
        const row = document.createElement('tr');
        const values = [word.word ?? word.text ?? '', formatSeconds(word.start), formatSeconds(word.end), word.speaker ?? '—'];
        for (const value of values) row.appendChild(Object.assign(document.createElement('td'), { textContent: String(value) }));
        body.appendChild(row);
      }
      table.appendChild(body);
      const wrap = document.createElement('div'); wrap.className = 'voice-word-table-wrap'; wrap.appendChild(table); result.appendChild(wrap);
    }

    window.addEventListener('grok-models-loaded', (event) => acceptRoutes(event.detail));
    const catalog = window.GrokModelCatalogPromise;
    if (catalog) catalog.then(acceptRoutes).catch((error) => setStatus(error.message, 'error'));
    el('audioOperation').addEventListener('change', refresh);
    el('audioModel').addEventListener('change', () => { if (el('audioOperation').value === 'tts') loadVoices(); });
    el('audioVoice').addEventListener('change', syncCustomVoice);
    el('imagineModel')?.addEventListener('change', () => { el('imagineResolution').disabled = !el('imagineModel').value; });
    el('audioSubmit').addEventListener('click', async () => {
      if (busy) return;
      const tts = el('audioOperation').value === 'tts';
      const model = el('audioModel').value;
      const language = el('audioLanguage').value.trim();
      busy = true; el('audioSubmit').disabled = true; setStatus('处理中…');
      try {
        let body, headers;
        if (tts) {
          const input = el('audioText').value.trim();
          if (!input) throw new Error('请输入文本');
          const voice = selectedVoice();
          if (!voice) throw new Error('请选择音色或输入 voice_id');
          body = JSON.stringify({ model, input, voice, voice_id: voice, language: language || undefined, speed: Number(el('voiceSpeed')?.value || 1), response_format: 'mp3' });
          headers = { 'Content-Type': 'application/json', Accept: 'audio/*, application/json' };
        } else {
          const file = el('audioFile').files[0];
          if (!file) throw new Error('请选择音频文件');
          body = new FormData(); body.set('model', model); body.set('file', file); body.set('response_format', 'verbose_json'); if (language) body.set('language', language);
        }
        const prefix = window.GrokToolRequest?.prefix?.() || '/grok/v1';
        const requestHeaders = window.GrokToolRequest?.headers?.(headers || {}) || headers;
        const response = await fetch(`${prefix}/audio/${tts ? 'speech' : 'transcriptions'}`, { method: 'POST', headers: requestHeaders, body });
        if (!response.ok) throw new Error(await responseError(response));
        clearObjectURL(); el('audioResult').replaceChildren();
        if (tts) {
          const contentType = response.headers.get('Content-Type') || '';
          let source;
          if (/json/i.test(contentType)) source = jsonAudioSource(await response.json(), contentType);
          else { const blob = await response.blob(); if (!blob.size) throw new Error('TTS 返回了空音频'); source = { blob, revoke: true }; }
          if (source.blob) { objectURL = URL.createObjectURL(source.blob); source.url = objectURL; }
          renderTTS(source, response);
        } else {
          const contentType = response.headers.get('Content-Type') || '';
          renderSTT(/json/i.test(contentType) ? await response.json() : { text: await response.text() });
        }
        setStatus('完成');
      } catch (error) { setStatus(error?.message || String(error), 'error'); }
      finally { busy = false; el('audioSubmit').disabled = !el('audioModel').value; }
    });
  }

  const mediaHistoryState = { imagePage: 1, videoPage: 1, pageSize: 12 };

  function escapeMediaHTML(value) {
    return String(value ?? '').replace(/[&<>"']/g, (character) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[character]);
  }

  function mediaPages(total) { return Math.max(1, Math.ceil(Number(total || 0) / mediaHistoryState.pageSize)); }

  async function mediaJSON(url) {
    const response = await fetch(url, { credentials: 'same-origin' });
    if (!response.ok) throw new Error(await response.text());
    return response.json();
  }

  async function loadAdminImages() {
    const host = document.getElementById('mediaImageGallery'); if (!host) return;
    const query = String(document.getElementById('mediaImageSearch')?.value || '').trim();
    const params = new URLSearchParams({ page: String(mediaHistoryState.imagePage), page_size: String(mediaHistoryState.pageSize) }); if (query) params.set('search', query);
    try {
      const [data, stats] = await Promise.all([mediaJSON(`/api/admin/v1/media/images?${params}`), mediaJSON('/api/admin/v1/media/images/stats')]);
      const items = Array.isArray(data.items) ? data.items : [];
      host.innerHTML = items.length ? items.map((item) => `<a class="admin-media-card" href="${escapeMediaHTML(item.view_url || item.url)}" target="_blank" rel="noopener"><img src="${escapeMediaHTML(item.preview_url || item.view_url || item.url)}" loading="lazy" alt="${escapeMediaHTML(item.name)}"><span>${escapeMediaHTML(item.name)}</span></a>`).join('') : '<div class="table-empty-cell">暂无图片</div>';
      const pages = mediaPages(data.total); mediaHistoryState.imagePage = Math.min(mediaHistoryState.imagePage, pages);
      document.getElementById('mediaImagePage').textContent = `${mediaHistoryState.imagePage} / ${pages}`;
      document.getElementById('mediaImagePrevBtn').disabled = mediaHistoryState.imagePage <= 1; document.getElementById('mediaImageNextBtn').disabled = mediaHistoryState.imagePage >= pages;
      document.getElementById('mediaImageStats').textContent = `${stats.count || 0} 张 · ${Number(stats.size_mb || 0).toFixed(2)} MB`;
    } catch (error) { host.innerHTML = `<div class="table-empty-cell">加载失败：${escapeMediaHTML(error.message)}</div>`; }
  }

  async function loadAdminVideos() {
    const body = document.getElementById('mediaVideoHistory'); if (!body) return;
    const params = new URLSearchParams({ page: String(mediaHistoryState.videoPage), page_size: String(mediaHistoryState.pageSize) });
    const search = String(document.getElementById('mediaVideoSearch')?.value || '').trim(), status = String(document.getElementById('mediaVideoStatus')?.value || '').trim(); if (search) params.set('search', search); if (status) params.set('status', status);
    try {
      const [data, stats] = await Promise.all([mediaJSON(`/api/admin/v1/media/videos?${params}`), mediaJSON('/api/admin/v1/media/videos/stats')]); const items = Array.isArray(data.items) ? data.items : [];
      body.innerHTML = items.length ? items.map((item) => `<tr><td><code>${escapeMediaHTML(item.id)}</code></td><td><span class="tag">${escapeMediaHTML(item.status)}</span></td><td><strong>${escapeMediaHTML(item.model || '-')}</strong><br><span class="meta-text">${escapeMediaHTML(item.prompt || item.error_message || '-')}</span></td><td>${Number(item.progress || 0)}%</td><td>${item.created_at ? new Date(Number(item.created_at) * 1000).toLocaleString() : '-'}</td></tr>`).join('') : '<tr><td colspan="5" class="table-empty-cell">暂无视频任务</td></tr>';
      const pages = mediaPages(data.total); mediaHistoryState.videoPage = Math.min(mediaHistoryState.videoPage, pages); document.getElementById('mediaVideoPage').textContent = `${mediaHistoryState.videoPage} / ${pages}`; document.getElementById('mediaVideoPrevBtn').disabled = mediaHistoryState.videoPage <= 1; document.getElementById('mediaVideoNextBtn').disabled = mediaHistoryState.videoPage >= pages;
      const statuses = stats.statuses || {}; document.getElementById('mediaVideoStats').textContent = `${stats.total || 0} 个任务` + Object.entries(statuses).map(([key, value]) => ` · ${key}: ${value}`).join('');
    } catch (error) { body.innerHTML = `<tr><td colspan="5" class="table-empty-cell">加载失败：${escapeMediaHTML(error.message)}</td></tr>`; }
  }

  function initAdminMediaHistory() {
    if (!document.getElementById('mediaImageGallery')) return;
    document.getElementById('mediaImageRefreshBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage = 1; loadAdminImages(); });
    document.getElementById('mediaVideoRefreshBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage = 1; loadAdminVideos(); });
    document.getElementById('mediaVideoStatus')?.addEventListener('change', () => { mediaHistoryState.videoPage = 1; loadAdminVideos(); });
    document.getElementById('mediaImagePrevBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage--; loadAdminImages(); }); document.getElementById('mediaImageNextBtn')?.addEventListener('click', () => { mediaHistoryState.imagePage++; loadAdminImages(); });
    document.getElementById('mediaVideoPrevBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage--; loadAdminVideos(); }); document.getElementById('mediaVideoNextBtn')?.addEventListener('click', () => { mediaHistoryState.videoPage++; loadAdminVideos(); });
    loadAdminImages(); loadAdminVideos();
  }

  if (typeof window !== 'undefined') window.GrokMediaTools = { normalizeVoices, jsonAudioSource, formatSeconds, loadAdminImages, loadAdminVideos };
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', () => { init(); initAdminMediaHistory(); }); else { init(); initAdminMediaHistory(); }
})();
