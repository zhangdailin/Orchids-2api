(() => {
  function init() {
    const host = document.querySelector('.voice-primary');
    if (!host || document.getElementById('voiceFileTools')) return;
    const section = document.createElement('section');
    section.id = 'voiceFileTools'; section.className = 'grok-panel voice-card';
    section.innerHTML = `<h3>语音生成与转写</h3>
      <label>操作<select id="audioOperation" class="form-input"><option value="tts">文本转语音</option><option value="stt">音频转文字</option></select></label>
      <label>模型<select id="audioModel" class="form-input"></select></label>
      <label>语言<input id="audioLanguage" class="form-input" value="zh"></label>
      <label id="audioTextLabel">文本<textarea id="audioText" class="form-input" rows="3"></textarea></label>
      <label id="audioFileLabel" hidden>音频文件<input id="audioFile" type="file" accept="audio/*"></label>
      <p>生成语音使用上方选择的音色和语速。</p>
      <button id="audioSubmit" class="btn btn-primary" type="button">执行</button>
      <p id="audioStatus" role="status"></p><div id="audioResult"></div>`;
    host.appendChild(section);
    const el = (id) => document.getElementById(id);
    let routes = [], objectURL = '', busy = false;
    function refresh() {
      const tts = el('audioOperation').value === 'tts';
      el('audioTextLabel').hidden = !tts; el('audioFileLabel').hidden = tts;
      const select = el('audioModel'), previous = select.value;
      select.replaceChildren();
      for (const route of routes.filter((r) => r.capabilities?.includes(tts ? 'tts' : 'stt'))) {
        const option = document.createElement('option'); option.value = route.id; option.textContent = route.id; select.appendChild(option);
      }
      if (Array.from(select.options).some((o) => o.value === previous)) select.value = previous;
      el('audioSubmit').disabled = busy || !select.value;
      if (!select.value) el('audioStatus').textContent = '没有配置支持此操作的模型';
      else el('audioStatus').textContent = '';
    }
    function acceptRoutes(value) {
      routes = value || []; refresh();
      const select = el('imagineModel');
      if (!select) return;
      const previous = select.value;
      while (select.options.length > 1) select.remove(1);
      for (const route of routes.filter((r) => r.capabilities?.includes('image'))) {
        const option = document.createElement('option'); option.value = route.id; option.textContent = route.id; select.appendChild(option);
      }
      select.value = previous;
    }
    window.addEventListener('grok-models-loaded', (event) => acceptRoutes(event.detail));
    fetch('/grok/v1/models').then((r) => { if (!r.ok) throw new Error('模型加载失败'); return r.json(); }).then((r) => acceptRoutes(r.data)).catch((e) => { el('audioStatus').textContent = e.message; });
    el('audioOperation').addEventListener('change', refresh);
    el('imagineModel')?.addEventListener('change', () => { el('imagineResolution').disabled = !el('imagineModel').value; });
    el('audioSubmit').addEventListener('click', async () => {
      if (busy) return;
      const tts = el('audioOperation').value === 'tts';
      const model = el('audioModel').value, language = el('audioLanguage').value.trim();
      busy = true; el('audioSubmit').disabled = true; el('audioStatus').textContent = '处理中…';
      try {
        let body, headers;
        if (tts) {
          const input = el('audioText').value.trim(); if (!input) throw new Error('请输入文本');
          const voice = el('voiceName').value === 'custom' ? el('voiceCustomID').value.trim() : el('voiceName').value;
          body = JSON.stringify({ model, input, voice, language, speed: Number(el('voiceSpeed').value), response_format: 'mp3' });
          headers = { 'Content-Type': 'application/json' };
        } else {
          const file = el('audioFile').files[0]; if (!file) throw new Error('请选择音频文件');
          body = new FormData(); body.set('model', model); body.set('file', file); if (language) body.set('language', language);
        }
        const response = await fetch(`/grok/v1/audio/${tts ? 'speech' : 'transcriptions'}`, { method: 'POST', headers, body });
        if (!response.ok) throw new Error(await response.text());
        const result = el('audioResult');
        if (objectURL) { URL.revokeObjectURL(objectURL); objectURL = ''; }
        result.replaceChildren();
        if (tts) {
          objectURL = URL.createObjectURL(await response.blob());
          const audio = document.createElement('audio'); audio.controls = true; audio.src = objectURL;
          const link = document.createElement('a'); link.href = objectURL; link.download = 'speech.mp3'; link.textContent = '下载语音';
          result.append(audio, link);
        } else { const data = await response.json(); result.textContent = data.text || ''; }
        el('audioStatus').textContent = '完成';
      } catch (error) { el('audioStatus').textContent = error.message; }
      finally { busy = false; el('audioSubmit').disabled = !el('audioModel').value; }
    });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init); else init();
})();
