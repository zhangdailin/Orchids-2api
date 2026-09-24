const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const source = fs.readFileSync(path.join(__dirname, 'static/js/grok-media-tools.js'), 'utf8');
const template = fs.readFileSync(path.join(__dirname, 'templates/pages/grok-tools.html'), 'utf8');
const styles = fs.readFileSync(path.join(__dirname, 'static/css/grok-tools.css'), 'utf8');

function helpers() {
  const window = {};
  const context = vm.createContext({
    window,
    document: { readyState: 'complete', getElementById: () => null },
    Blob,
    Uint8Array,
    atob,
  });
  vm.runInContext(source, context);
  return window.GrokMediaTools;
}

test('media tools render in the voice page without replacing realtime controls', () => {
  assert.match(template, /id="voiceFileTools"/);
  assert.match(template, /id="voiceStartBtn"/);
  assert.match(template, /id="audioVoice"/);
  assert.match(template, /id="audioResult"/);
  assert.match(styles, /\.voice-file-grid\s*\{/);
  assert.match(styles, /\.voice-word-table-wrap\s*\{/);
});

test('voice catalog normalization accepts native and standard list shapes', () => {
  const { normalizeVoices } = helpers();
  const native = normalizeVoices({ voices: [{ voice_id: 'ara', name: 'Ara', language: 'en' }] });
  assert.deepEqual(JSON.parse(JSON.stringify(native)), [{ id: 'ara', name: 'Ara', language: 'en' }]);
  const standard = normalizeVoices({ data: [{ id: 'custom-1', display_name: 'Custom', locale: 'zh-CN' }, 'eve'] });
  assert.deepEqual(JSON.parse(JSON.stringify(standard)), [
    { id: 'custom-1', name: 'Custom', language: 'zh-CN' },
    { id: 'eve', name: 'eve', language: '' },
  ]);
});

test('JSON TTS responses support URLs and base64 audio', async () => {
  const { jsonAudioSource } = helpers();
  assert.deepEqual(JSON.parse(JSON.stringify(jsonAudioSource({ audio_url: 'https://example.test/speech.mp3' }))), {
    url: 'https://example.test/speech.mp3', revoke: false,
  });
  const source = jsonAudioSource({ audio_base64: Buffer.from('audio').toString('base64'), format: 'wav' }, 'application/json');
  assert.equal(source.revoke, true);
  assert.equal(source.blob.type, 'audio/wav');
  assert.equal(await source.blob.text(), 'audio');
  assert.throws(() => jsonAudioSource({}, 'application/json'), /没有音频数据/);
});

test('requests use model-scoped voice discovery and verbose STT metadata', () => {
  assert.match(source, /tts\/voices/);
  assert.match(source, /\/v1\/tts\/voices/);
  assert.match(source, /model=\$\{encodeURIComponent\(model\)\}/);
  assert.match(source, /body\.set\('response_format', 'verbose_json'\)/);
  assert.match(source, /data\?\.language/);
  assert.match(source, /data\?\.duration/);
  assert.match(source, /word\.speaker/);
});

test('cache media gallery uses authenticated admin history endpoints', () => {
  assert.match(template, /id="mediaImageGallery"/);
  assert.match(template, /id="mediaVideoHistory"/);
  assert.match(source, /\/api\/admin\/v1\/media\/images\?/);
  assert.match(source, /\/api\/admin\/v1\/media\/images\/stats/);
  assert.match(source, /\/api\/admin\/v1\/media\/videos\?/);
  assert.match(source, /\/api\/admin\/v1\/media\/videos\/stats/);
  assert.match(source, /credentials:\s*'same-origin'/);
  assert.match(styles, /\.admin-media-gallery\s*\{/);
});
