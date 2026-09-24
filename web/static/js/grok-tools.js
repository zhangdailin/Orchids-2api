(() => {
  const toolAuthState = { mode: "admin", apiKey: "" };
  function toolAuthHeaders(headers = {}) {
    return toolAuthState.mode === "client" && toolAuthState.apiKey
      ? { ...headers, Authorization: `Bearer ${toolAuthState.apiKey}` }
      : headers;
  }
  function toolInferencePrefix() { return toolAuthState.mode === "client" ? "/v1" : "/api/grok/tools/v1"; }
  function toolHistoryScope() {
    if (!toolAuthState.apiKey) return "admin";
    let hash = 2166136261;
    for (const char of toolAuthState.apiKey) hash = Math.imul(hash ^ char.charCodeAt(0), 16777619);
    return `key-${(hash >>> 0).toString(16)}`;
  }

  if (typeof window !== "undefined") {
    window.GrokToolRequest = Object.freeze({ headers: (headers = {}) => toolAuthHeaders(headers), prefix: () => toolInferencePrefix() });
  }


  const chatState = {
    sessions: [], activeId: "", sending: false, abortController: null, renderFrame: 0,
    sidebarOpen: false, model: "", models: [], routes: [], capabilities: { chat: false },
    modelsLoaded: false, requestGeneration: 0, persistenceTimer: null,
  };
  const chatStorageKey = "grok_tools_chat_sessions_v1";
  const chatSidebarStateKey = "grok_tools_chat_sidebar_collapsed";
  const grokToolsUIStorageKey = "grok_tools_ui_v1";
  function handleUnauthorized(res) {
    if (!res || res.status !== 401) return false;
    // Only the admin session middleware answers a bare plain-text 401. A JSON
    // 401 comes from a Grok handler — for example every upstream account needing
    // re-login, or a rejected client key — and is a tool failure, not an expired
    // console session. Redirecting on that logged the operator out of a page
    // that was still authenticated, which made the tools look unusable.
    const contentType = String(res.headers?.get?.("Content-Type") || "").toLowerCase();
    const sessionExpired = contentType.includes("text/plain");
    if (sessionExpired) {
      window.location.href = "/admin/login.html?next=" + encodeURIComponent("/admin/?tab=grok-tools");
    }
    return true;
  }

  function formatDateTime(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    return new Date(num).toLocaleString();
  }

  function relativeTime(ms) {
    const num = Number(ms || 0);
    if (!Number.isFinite(num) || num <= 0) return "-";
    const diff = Math.max(0, Math.round((Date.now() - num) / 1000));
    if (diff < 60) return "刚刚";
    if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`;
    if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`;
    return `${Math.floor(diff / 86400)} 天前`;
  }

  function loadGrokToolsUIState() {
    try {
      const raw = localStorage.getItem(grokToolsUIStorageKey);
      if (!raw) return {};
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === "object" ? parsed : {};
    } catch (err) {
      return {};
    }
  }

  function saveGrokToolsUIState(patch) {
    try {
      const current = loadGrokToolsUIState();
      localStorage.setItem(grokToolsUIStorageKey, JSON.stringify({ ...current, ...patch }));
    } catch (err) {
      // ignore storage failures
    }
  }

  function setChatSendButtonState(sending) {
    const btn = document.getElementById("grokSendBtn");
    if (!btn) return;
    btn.disabled = false;
    if (sending) {
      btn.title = "停止生成";
      btn.innerHTML = `
        <svg width="14" height="14" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true">
          <rect x="6" y="6" width="12" height="12" rx="2"></rect>
        </svg>
      `;
      return;
    }
    btn.title = "发送";
    btn.innerHTML = `
      <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" aria-hidden="true">
        <path d="M22 2L11 13"></path>
        <path d="M22 2L15 22L11 13L2 9L22 2Z"></path>
      </svg>
    `;
  }

  function setChatSidebarCollapsed(collapsed) {
    const layout = document.querySelector("#grokChatSection .chat-layout");
    if (!layout) return;
    layout.classList.toggle("collapsed", collapsed);
    try {
      localStorage.setItem(chatSidebarStateKey, collapsed ? "1" : "0");
    } catch (err) {
      // ignore storage failures
    }
  }

  function closeChatSidebar() {
    if (isMobileChatSidebar()) {
      chatState.sidebarOpen = false;
      const sidebar = document.getElementById("grokChatSidebar");
      const overlay = document.getElementById("grokChatSidebarOverlay");
      sidebar?.classList.remove("open");
      sidebar?.classList.remove("show");
      overlay?.classList.remove("open");
      overlay?.classList.remove("show");
      return;
    }
    setChatSidebarCollapsed(true);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = "inline-flex";
  }

  function openChatSidebar() {
    if (isMobileChatSidebar()) {
      chatState.sidebarOpen = true;
      const sidebar = document.getElementById("grokChatSidebar");
      const overlay = document.getElementById("grokChatSidebarOverlay");
      sidebar?.classList.add("open");
      sidebar?.classList.add("show");
      overlay?.classList.add("open");
      overlay?.classList.add("show");
      return;
    }
    setChatSidebarCollapsed(false);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = "none";
  }

  function toggleChatSidebar() {
    if (isMobileChatSidebar()) {
      const sidebar = document.getElementById("grokChatSidebar");
      if (sidebar?.classList.contains("open") || sidebar?.classList.contains("show")) {
        closeChatSidebar();
      } else {
        openChatSidebar();
      }
      return;
    }
    const layout = document.querySelector("#grokChatSection .chat-layout");
    if (!layout) return;
    if (layout.classList.contains("collapsed")) {
      openChatSidebar();
    } else {
      closeChatSidebar();
    }
  }

  function isMobileChatSidebar() {
    return window.matchMedia("(max-width: 1024px)").matches;
  }

  function syncChatSidebarState() {
    if (isMobileChatSidebar()) {
      if (chatState.sidebarOpen) {
        openChatSidebar();
      } else {
        closeChatSidebar();
      }
      return;
    }
    const sidebar = document.getElementById("grokChatSidebar");
    const overlay = document.getElementById("grokChatSidebarOverlay");
    sidebar?.classList.remove("open");
    sidebar?.classList.remove("show");
    overlay?.classList.remove("open");
    overlay?.classList.remove("show");
    let collapsed = false;
    try {
      collapsed = localStorage.getItem(chatSidebarStateKey) === "1";
    } catch (err) {
      collapsed = false;
    }
    setChatSidebarCollapsed(collapsed);
    const expandBtn = document.getElementById("grokChatExpandBtn");
    if (expandBtn) expandBtn.style.display = collapsed ? "inline-flex" : "none";
  }

  function createChatSession() {
    return {
      id: `${Date.now().toString(36)}${Math.random().toString(36).slice(2, 8)}`,
      title: "新会话",
      isDefaultTitle: true,
      createdAt: Date.now(),
      updatedAt: Date.now(),
      messages: [],
      model: chatState.model,
      promptCacheKey: createPromptCacheKey(),
      reasoningEffort: "",
      webSearch: false,
      xSearch: false,
    };
  }

  function createPromptCacheKey() {
    if (globalThis.crypto && typeof globalThis.crypto.randomUUID === "function") return `grok-tools-${globalThis.crypto.randomUUID()}`;
    return `grok-tools-${Date.now().toString(36)}-${Math.random().toString(36).slice(2, 10)}`;
  }

  function normalizeAssistantMessage(message) {
    if (message.role !== "assistant" || !/<think>/i.test(String(message.content || ""))) return message;
    // New-format messages store reasoning separately; content is the pure answer and must never be re-scanned.
    if (String(message.reasoning || "").trim()) return message;
    // Legacy history always led with a reasoning block. Mid-body think tags are real content (e.g. code examples).
    const raw = String(message.content || "");
    if (!/^\s*<think>/i.test(raw)) return message;
    const thoughts = [];
    const content = raw.replace(/<think>([\s\S]*?)(?:<\/think>|$)/gi, (_, text) => {
      thoughts.push(text.trim());
      return "";
    });
    return { ...message, content: content.trim(), reasoning: [message.reasoning || "", ...thoughts].filter(Boolean).join("\n\n") };
  }

  function assistantDisplay(message) {
    const reasoning = String(message.reasoning || "");
    return (reasoning ? `<think>${reasoning}</think>` : "") + String(message.content || "");
  }

  function renderToolActivity(tools) {
    return (tools || []).map((tool) => `<div class="tool-activity">${escapeHtml(tool.name || tool.type || "工具")} · ${escapeHtml(tool.status || "in_progress")}${tool.detail ? `<pre>${escapeHtml(tool.detail)}</pre>` : ""}</div>`).join("");
  }

  const chatSessionLimit = 50;
  const chatStorageByteLimit = 4 * 1024 * 1024;
  const chatPersistenceDelay = 250;

  function scopedChatStorageKey() {
    return `${chatStorageKey}:${toolHistoryScope()}`;
  }

  function persistedChatSession(session) {
    return {
      ...session,
      messages: Array.isArray(session.messages)
        ? session.messages.map((msg) => ({
            ...msg,
            attachment: msg?.attachment
              ? {
                  name: String(msg.attachment.name || ""),
                  type: String(msg.attachment.type || ""),
                  dataUrl: msg.attachment.dataUrl,
                }
              : undefined,
          }))
        : [],
    };
  }

  function chatPayloadJSON(sessions) {
    return JSON.stringify({ activeId: chatState.activeId, model: chatState.model, sessions });
  }

  function storageByteLength(value) {
    if (typeof TextEncoder === "function") return new TextEncoder().encode(value).byteLength;
    return unescape(encodeURIComponent(value)).length;
  }

  function boundedChatPayload() {
    const sessions = chatState.sessions.filter(Boolean).map(persistedChatSession);
    sessions.sort((a, b) => Number(b.updatedAt || b.createdAt || 0) - Number(a.updatedAt || a.createdAt || 0));
    if (sessions.length > chatSessionLimit) sessions.length = chatSessionLimit;
    let payload = chatPayloadJSON(sessions);
    while (sessions.length && storageByteLength(payload) > chatStorageByteLimit) {
      sessions.pop();
      payload = chatPayloadJSON(sessions);
    }
    return { payload, sessions };
  }

  function persistChatSessionsNow() {
    if (chatState.persistenceTimer) {
      clearTimeout(chatState.persistenceTimer);
      chatState.persistenceTimer = null;
    }
    try {
      const bounded = boundedChatPayload();
      // Keep memory and disk subject to the same limits, so an evicted session
      // cannot reappear during the next save in this tab.
      chatState.sessions = bounded.sessions;
      if (!chatState.sessions.some((item) => item.id === chatState.activeId)) {
        chatState.activeId = chatState.sessions[0]?.id || "";
      }
      localStorage.setItem(scopedChatStorageKey(), chatPayloadJSON(chatState.sessions));
    } catch (err) {
      updateChatStatus("浏览器存储空间不足，会话尚未保存；请释放存储空间后重试", "error");
    }
  }

  function saveChatSessions() {
    if (chatState.persistenceTimer) clearTimeout(chatState.persistenceTimer);
    chatState.persistenceTimer = setTimeout(persistChatSessionsNow, chatPersistenceDelay);
  }

  function loadChatSessions() {
    try {
      const storageKey = scopedChatStorageKey();
      // Migrate the original unscoped history only into the admin scope. A
      // selected client key receives a distinct non-secret fingerprint scope.
      const raw = localStorage.getItem(storageKey)
        || (toolHistoryScope() === "admin" ? localStorage.getItem(chatStorageKey) : null);
      if (raw) {
        const parsed = JSON.parse(raw);
        if (parsed && Array.isArray(parsed.sessions)) {
          chatState.sessions = parsed.sessions;
          chatState.activeId = String(parsed.activeId || "");
          if (typeof parsed.model === "string" && parsed.model.trim()) {
            chatState.model = parsed.model.trim();
          }
        }
      }
    } catch (err) {
      chatState.sessions = [];
      chatState.activeId = "";
    }
    if (!Array.isArray(chatState.sessions) || chatState.sessions.length === 0) {
      const session = createChatSession();
      chatState.sessions = [session];
      chatState.activeId = session.id;
    }
    if (!chatState.sessions.find((item) => item && item.id === chatState.activeId)) {
      chatState.activeId = chatState.sessions[0].id;
    }
    chatState.sessions.forEach((session) => {
      if (session && typeof session.isDefaultTitle === "undefined") {
        session.isDefaultTitle = !session.title || session.title === "新会话";
      }
      if (session && !session.promptCacheKey) session.promptCacheKey = createPromptCacheKey();
      if (session && typeof session.reasoningEffort !== "string") session.reasoningEffort = "";
      if (session) session.webSearch = session.webSearch === true;
      if (session) session.xSearch = session.xSearch === true;
      if (Array.isArray(session?.messages)) session.messages = session.messages.map(normalizeAssistantMessage);
    });
    // Loading old/unbounded data also repairs it in the scoped store.
    saveChatSessions();
  }

  function activeChatSession() {
    return chatState.sessions.find((item) => item && item.id === chatState.activeId) || null;
  }

  function updateChatStatus(text, type) {
    const el = document.getElementById("grokChatStatus");
    if (!el) return;
    el.textContent = String(text || "");
    el.classList.remove("connected", "connecting", "error");
    if (type === "ok") el.classList.add("connected");
    if (type === "connecting") el.classList.add("connecting");
    if (type === "error") el.classList.add("error");
  }

  const escapeHTML = escapeHtml;

  function setRenderedHTML(el, html) {
    el.innerHTML = html;
  }

  function isSafeLinkURL(url) {
    const value = String(url || "").trim().toLowerCase();
    if (!value) return false;
    return /^(https?:|mailto:|tel:|\/(?!\/)|\.\.?\/|#)/.test(value);
  }

  function isSafeImageURL(url) {
    const value = String(url || "").trim().toLowerCase();
    if (!value) return false;
    return /^(https?:|data:image\/(?:png|jpe?g|gif|webp|bmp|ico);base64,|\/(?!\/)|\.\.?\/)/.test(value);
  }

  function renderBasicMarkdown(rawText) {
    const text = String(rawText || "").replace(/\\n/g, "\n");
    const escaped = escapeHTML(text);
    const codeBlocks = [];
    const fenced = escaped.replace(/```([a-zA-Z0-9_-]+)?\n([\s\S]*?)```/g, (_match, lang, code) => {
      const safeLang = lang ? escapeHTML(lang) : "";
      const html = `<pre class="code-block"><code${safeLang ? ` class="language-${safeLang}"` : ""}>${code}</code></pre>`;
      const token = `@@CODEBLOCK_${codeBlocks.length}@@`;
      codeBlocks.push(html);
      return token;
    });

    const renderInline = (value) => {
      const inlineCodes = [];
      let output = value.replace(/`([^`]+)`/g, (_m, code) => {
        const token = `@@INLINE_${inlineCodes.length}@@`;
        inlineCodes.push(`<code class="inline-code">${code}</code>`);
        return token;
      });
      output = output
        .replace(/\*\*([^*]+)\*\*/g, "<strong>$1</strong>")
        .replace(/\*([^*]+)\*/g, "<em>$1</em>")
        .replace(/~~([^~]+)~~/g, "<del>$1</del>");
      output = output.replace(/!\[([^\]]*)\]\(([^)]+)\)/g, (_m, alt, url) => {
        const safeAlt = escapeHTML(alt || "image");
        if (!isSafeImageURL(url)) return safeAlt;
        return `<img src="${escapeHTML(url || "")}" alt="${safeAlt}" loading="lazy">`;
      });
      output = output.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_m, label, url) => {
        const safeLabel = escapeHTML(label || "");
        if (!isSafeLinkURL(url)) return safeLabel;
        return `<a href="${escapeHTML(url || "")}" target="_blank" rel="noopener">${safeLabel}</a>`;
      });
      output = output.replace(/(data:image\/[a-zA-Z0-9.+-]+;base64,[A-Za-z0-9+/=]+)/g, (uri) => {
        if (!isSafeImageURL(uri)) return "";
        return `<img src="${escapeHTML(uri)}" alt="image" loading="lazy">`;
      });
      inlineCodes.forEach((html, index) => {
        output = output.replace(new RegExp(`@@INLINE_${index}@@`, "g"), html);
      });
      return output;
    };

    const lines = fenced.split(/\r?\n/);
    const htmlParts = [];
    let inUl = false;
    let inTaskUl = false;
    let inOl = false;
    let inTable = false;
    let paragraphLines = [];

    const closeLists = () => {
      if (inUl) {
        htmlParts.push("</ul>");
        inUl = false;
        inTaskUl = false;
      }
      if (inOl) {
        htmlParts.push("</ol>");
        inOl = false;
      }
    };
    const closeTable = () => {
      if (inTable) {
        htmlParts.push("</tbody></table></div>");
        inTable = false;
      }
    };
    const flushParagraph = () => {
      if (!paragraphLines.length) return;
      htmlParts.push(`<p>${renderInline(paragraphLines.join("<br>"))}</p>`);
      paragraphLines = [];
    };
    const isTableSeparator = (line) => /^\s*\|?(?:\s*:?-+:?\s*\|)+\s*$/.test(line);
    const splitTableRow = (line) => line.trim().replace(/^\|/, "").replace(/\|$/, "").split("|").map((cell) => cell.trim());

    for (let i = 0; i < lines.length; i += 1) {
      const line = lines[i];
      const trimmed = line.trim();
      if (!trimmed) {
        flushParagraph();
        closeLists();
        closeTable();
        continue;
      }

      const codeTokenMatch = trimmed.match(/^@@CODEBLOCK_(\d+)@@$/);
      if (codeTokenMatch) {
        flushParagraph();
        closeLists();
        closeTable();
        htmlParts.push(trimmed);
        continue;
      }

      const headingMatch = trimmed.match(/^(#{1,6})\s+(.*)$/);
      if (headingMatch) {
        flushParagraph();
        closeLists();
        closeTable();
        const level = headingMatch[1].length;
        htmlParts.push(`<h${level}>${renderInline(headingMatch[2])}</h${level}>`);
        continue;
      }

      if (/^(-{3,}|\*{3,}|_{3,})$/.test(trimmed)) {
        flushParagraph();
        closeLists();
        closeTable();
        htmlParts.push("<hr>");
        continue;
      }

      if (/^\s*>/.test(line)) {
        flushParagraph();
        closeLists();
        closeTable();
        const quoteLines = [];
        let j = i;
        for (; j < lines.length; j += 1) {
          const currentLine = lines[j];
          if (!/^\s*>/.test(currentLine)) break;
          quoteLines.push(currentLine.replace(/^\s*>\s?/, ""));
        }
        i = j - 1;
        htmlParts.push(`<blockquote>${renderBasicMarkdown(quoteLines.join("\n"))}</blockquote>`);
        continue;
      }

      if (trimmed.includes("|")) {
        const nextLine = lines[i + 1] || "";
        if (!inTable && isTableSeparator(nextLine.trim())) {
          flushParagraph();
          closeLists();
          const headers = splitTableRow(trimmed);
          htmlParts.push('<div class="table-wrap"><table><thead><tr>');
          headers.forEach((cell) => htmlParts.push(`<th>${renderInline(cell)}</th>`));
          htmlParts.push("</tr></thead><tbody>");
          inTable = true;
          i += 1;
          continue;
        }
        if (inTable && !isTableSeparator(trimmed)) {
          const cells = splitTableRow(trimmed);
          htmlParts.push("<tr>");
          cells.forEach((cell) => htmlParts.push(`<td>${renderInline(cell)}</td>`));
          htmlParts.push("</tr>");
          continue;
        }
      }

      const taskMatch = trimmed.match(/^[-*+•·]\s+\[([ xX])\]\s+(.*)$/);
      if (taskMatch) {
        flushParagraph();
        if (inUl && !inTaskUl) closeLists();
        if (!inUl) {
          closeLists();
          closeTable();
          htmlParts.push('<ul class="task-list">');
          inUl = true;
          inTaskUl = true;
        }
        const checked = taskMatch[1].toLowerCase() === "x";
        htmlParts.push(`<li class="task-item"><input type="checkbox" disabled${checked ? " checked" : ""}>${renderInline(taskMatch[2])}</li>`);
        continue;
      }

      const ulMatch = trimmed.match(/^[-*+•·]\s+(.*)$/);
      if (ulMatch) {
        flushParagraph();
        if (!inUl) {
          closeLists();
          closeTable();
          htmlParts.push("<ul>");
          inUl = true;
          inTaskUl = false;
        }
        htmlParts.push(`<li>${renderInline(ulMatch[1])}</li>`);
        continue;
      }

      const olMatch = trimmed.match(/^\d+[.)、]\s+(.*)$/);
      if (olMatch) {
        flushParagraph();
        if (!inOl) {
          closeLists();
          closeTable();
          htmlParts.push("<ol>");
          inOl = true;
        }
        htmlParts.push(`<li>${renderInline(olMatch[1])}</li>`);
        continue;
      }

      paragraphLines.push(trimmed);
    }

    flushParagraph();
    closeLists();
    closeTable();

    let output = htmlParts.join("");
    codeBlocks.forEach((html, index) => {
      output = output.replace(`@@CODEBLOCK_${index}@@`, html);
    });
    return output;
  }

  function parseThinkSections(raw) {
    const input = String(raw || "");
    const parts = [];
    let cursor = 0;
    while (cursor < input.length) {
      const start = input.indexOf("<think>", cursor);
      if (start === -1) {
        parts.push({ type: "text", value: input.slice(cursor) });
        break;
      }
      if (start > cursor) {
        parts.push({ type: "text", value: input.slice(cursor, start) });
      }
      const thinkStart = start + 7;
      const end = input.indexOf("</think>", thinkStart);
      if (end === -1) {
        parts.push({ type: "think", value: input.slice(thinkStart), open: true });
        cursor = input.length;
      } else {
        parts.push({ type: "think", value: input.slice(thinkStart, end), open: false });
        cursor = end + 8;
      }
    }
    return parts;
  }

  function parseRolloutBlocks(text) {
    const lines = String(text || "").split(/\r?\n/);
    const blocks = [];
    let current = null;
    for (const line of lines) {
      const match = line.match(/^\s*\[([^\]]+)\]\[([^\]]+)\]\s*(.*)$/);
      if (match) {
        if (current) blocks.push(current);
        current = { id: match[1], type: match[2], lines: [] };
        if (match[3]) current.lines.push(match[3]);
        continue;
      }
      if (current) {
        current.lines.push(line);
      }
    }
    if (current) blocks.push(current);
    return blocks;
  }

  function parseAgentSections(text) {
    const lines = String(text || "").split(/\r?\n/);
    const sections = [];
    let current = { title: null, lines: [] };
    let hasAgentHeading = false;
    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) {
        current.lines.push(line);
        continue;
      }
      const agentMatch = trimmed.match(/^(Grok\s+Leader|(?:Grok\s+)?Agent\s*\d+)$/i);
      if (agentMatch) {
        hasAgentHeading = true;
        if (current.lines.length) {
          sections.push(current);
        }
        current = { title: agentMatch[1], lines: [] };
        continue;
      }
      current.lines.push(line);
    }
    if (current.lines.length) {
      sections.push(current);
    }
    if (!hasAgentHeading) {
      return [{ title: null, lines }];
    }
    return sections;
  }

  const toolTypeMap = {
    websearch: { icon: "", label: "网页搜索" },
    searchimage: { icon: "", label: "图片搜索" },
    agentthink: { icon: "", label: "Agent Think" },
  };
  const defaultToolType = { icon: "", label: "工具" };

  function getToolMeta(typeStr) {
    const key = String(typeStr || "").trim().toLowerCase().replace(/\s+/g, "");
    return toolTypeMap[key] || defaultToolType;
  }

  function renderThinkContent(text, openAll) {
    const sections = parseAgentSections(text);
    if (!sections.length) {
      return renderBasicMarkdown(text);
    }
    const renderGroups = (blocks, openAllGroups) => {
      const groups = [];
      const map = new Map();
      for (const block of blocks) {
        const key = block.id;
        let group = map.get(key);
        if (!group) {
          group = { id: key, items: [] };
          map.set(key, group);
          groups.push(group);
        }
        group.items.push(block);
      }
      return groups.map((group) => {
        const items = group.items.map((item) => {
          const body = renderBasicMarkdown(item.lines.join("\n").trim());
          const typeKey = String(item.type || "").trim().toLowerCase().replace(/\s+/g, "");
          const typeAttr = escapeHTML(typeKey);
          const meta = getToolMeta(item.type);
          const iconHtml = meta.icon ? `<span class="think-tool-icon">${meta.icon}</span>` : "";
          const typeLabel = `${iconHtml}${escapeHTML(meta.label)}`;
          return `<div class="think-item-row think-tool-card" data-tool-type="${typeAttr}"><div class="think-item-type" data-type="${typeAttr}">${typeLabel}</div><div class="think-item-body">${body || "<em>空</em>"}</div></div>`;
        }).join("");
        const title = escapeHTML(group.id);
        const openAttr = openAllGroups ? " open" : "";
        return `<details class="think-rollout-group"${openAttr}><summary><span class="think-rollout-title">${title}</span></summary><div class="think-rollout-body">${items}</div></details>`;
      }).join("");
    };

    const agentBlocks = sections.map((section, idx) => {
      const blocks = parseRolloutBlocks(section.lines.join("\n"));
      const inner = blocks.length
        ? renderGroups(blocks, openAll)
        : `<div class="think-rollout-body">${renderBasicMarkdown(section.lines.join("\n").trim())}</div>`;
      if (!section.title) {
        return `<div class="think-agent-items">${inner}</div>`;
      }
      const title = escapeHTML(section.title);
      const openAttr = openAll ? " open" : (idx === 0 ? " open" : "");
      return `<details class="think-agent"${openAttr}><summary>${title}</summary><div class="think-agent-items">${inner}</div></details>`;
    });
    return `<div class="think-agents">${agentBlocks.join("")}</div>`;
  }

  function renderAssistantContent(raw) {
    const parts = parseThinkSections(String(raw || ""));
    return parts.map((part) => {
      if (part.type === "think") {
        const body = renderThinkContent(part.value.trim(), part.open);
        const openAttr = part.open ? " open" : "";
        return `<details class="think-block" data-think="true"${openAttr}><summary class="think-summary">思考过程</summary><div class="think-content">${body || "<em>空</em>"}</div></details>`;
      }
      return renderBasicMarkdown(part.value);
    }).join("");
  }

  function applyImageGrid(root) {
    if (!root) return;
    const isIgnorable = (node) => {
      if (node.nodeType === Node.TEXT_NODE) {
        return !node.textContent.trim();
      }
      return node.nodeType === Node.ELEMENT_NODE && node.tagName === "BR";
    };

    const isImageLink = (node) => {
      if (!node || node.nodeType !== Node.ELEMENT_NODE || node.tagName !== "A") return false;
      const children = Array.from(node.childNodes);
      if (!children.length) return false;
      return children.every((child) => {
        if (child.nodeType === Node.TEXT_NODE) {
          return !child.textContent.trim();
        }
        return child.nodeType === Node.ELEMENT_NODE && child.tagName === "IMG";
      });
    };

    const extractImageItems = (node) => {
      if (!node || node.nodeType !== Node.ELEMENT_NODE) return null;
      if (node.classList && node.classList.contains("img-grid")) return null;
      if (node.tagName === "IMG") {
        return { items: [node], removeNode: null };
      }
      if (isImageLink(node)) {
        return { items: [node], removeNode: null };
      }
      if (node.tagName === "P") {
        const items = [];
        const children = Array.from(node.childNodes);
        if (!children.length) return null;
        for (const child of children) {
          if (child.nodeType === Node.TEXT_NODE) {
            if (!child.textContent.trim()) continue;
            return null;
          }
          if (child.nodeType === Node.ELEMENT_NODE) {
            if (child.tagName === "IMG" || isImageLink(child)) {
              items.push(child);
              continue;
            }
            if (child.tagName === "BR") continue;
            return null;
          }
          return null;
        }
        if (!items.length) return null;
        return { items, removeNode: node };
      }
      return null;
    };

    const wrapImagesInContainer = (container) => {
      const children = Array.from(container.childNodes);
      let group = [];
      let groupStart = null;
      let removeNodes = [];

      const flush = () => {
        if (group.length < 2) {
          group = [];
          groupStart = null;
          removeNodes = [];
          return;
        }
        const wrapper = document.createElement("div");
        wrapper.className = "img-grid";
        const cols = Math.min(4, group.length);
        wrapper.style.setProperty("--cols", String(cols));
        if (groupStart) {
          container.insertBefore(wrapper, groupStart);
        } else {
          container.appendChild(wrapper);
        }
        group.forEach((img) => wrapper.appendChild(img));
        removeNodes.forEach((n) => n.parentNode && n.parentNode.removeChild(n));
        group = [];
        groupStart = null;
        removeNodes = [];
      };

      children.forEach((node) => {
        if (group.length && isIgnorable(node)) {
          removeNodes.push(node);
          return;
        }
        const extracted = extractImageItems(node);
        if (extracted && extracted.items.length) {
          if (!groupStart) groupStart = node;
          group.push(...extracted.items);
          if (extracted.removeNode) {
            removeNodes.push(extracted.removeNode);
          }
          return;
        }
        flush();
      });
      flush();
    };

    const containers = [root, ...root.querySelectorAll(".think-content, .think-item-body, .think-rollout-body, .think-agent-items")];
    containers.forEach((container) => {
      if (!container || container.closest(".img-grid")) return;
      if (!container.querySelector || !container.querySelector("img")) return;
      wrapImagesInContainer(container);
    });
  }

  function enhanceBrokenImages(root) {
    if (!root) return;
    const images = root.querySelectorAll("img");
    images.forEach((img) => {
      if (img.dataset.retryBound) return;
      img.dataset.retryBound = "1";
      img.addEventListener("error", () => {
        if (img.dataset.failed) return;
        img.dataset.failed = "1";
        const wrapper = document.createElement("button");
        wrapper.type = "button";
        wrapper.className = "img-retry";
        wrapper.textContent = "点击重试";
        wrapper.addEventListener("click", () => {
          wrapper.classList.add("loading");
          const original = img.getAttribute("src") || "";
          const cacheBust = original.includes("?") ? "&" : "?";
          img.dataset.failed = "";
          img.src = `${original}${cacheBust}t=${Date.now()}`;
        });
        img.replaceWith(wrapper);
      });
      img.addEventListener("load", () => {
        if (img.dataset.failed) {
          img.dataset.failed = "";
        }
      });
    });
  }

  function updateThinkSummary(root, elapsedSec) {
    if (!root) return;
    const summaries = root.querySelectorAll(".think-summary");
    if (!summaries.length) return;
    let text = "思考过程";
    if (typeof elapsedSec === "number") {
      text = elapsedSec > 0 ? `思考 ${elapsedSec}s` : "思考完成";
    } else {
      text = "思考中";
    }
    summaries.forEach((node) => {
      node.textContent = text;
      const block = node.closest(".think-block");
      if (!block) return;
      if (typeof elapsedSec === "number") {
        block.removeAttribute("data-thinking");
      } else {
        block.setAttribute("data-thinking", "true");
      }
    });
  }

  function renderUserContent(content, attachment) {
    const wrapper = document.createElement("div");
    const text = document.createElement("div");
    text.className = "message-content";
    text.textContent = String(content || "");
    wrapper.appendChild(text);
    if (attachment && attachment.name) {
      const badge = document.createElement("div");
      badge.className = "message-attachment";
      badge.textContent = `附件: ${attachment.name}`;
      wrapper.appendChild(badge);
    }
    return wrapper;
  }

  function appendChatMessage(role, content, attachment, message) {
    const log = document.getElementById("grokChatLog");
    const empty = document.getElementById("grokChatEmpty");
    if (!log) return null;
    if (empty) empty.style.display = "none";
    const row = document.createElement("div");
    row.className = `message-row ${role}`;
    const bubble = document.createElement("div");
    bubble.className = "message-bubble";
    let contentEl = document.createElement("div");
    contentEl.className = role === "assistant" ? "message-content rendered" : "message-content";
    if (role === "assistant") {
      setRenderedHTML(contentEl, renderAssistantContent(message ? assistantDisplay(message) : content || "") + renderToolActivity(message?.tools));
      applyImageGrid(contentEl);
      if (String(content || "").includes("<think>")) {
        updateThinkSummary(contentEl, 0);
      }
      enhanceBrokenImages(contentEl);
      bubble.appendChild(contentEl);
    } else {
      contentEl = renderUserContent(content, attachment);
      bubble.appendChild(contentEl);
    }
    row.appendChild(bubble);
    const actions = document.createElement("div");
    actions.className = "message-actions";
    const copyBtn = document.createElement("button");
    copyBtn.type = "button";
    copyBtn.className = "action-btn";
    copyBtn.textContent = "复制";
    copyBtn.addEventListener("click", () => {
      const text = role === "assistant"
        ? String(bubble.innerText || "").trim()
        : String(content || "");
      copyToClipboard(text);
    });
    actions.appendChild(copyBtn);
    if (role === "user") {
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "action-btn";
      editBtn.textContent = "编辑";
      editBtn.addEventListener("click", () => startEditChatMessage(row, content, attachment));
      actions.appendChild(editBtn);
    } else {
      const retryBtn = document.createElement("button");
      retryBtn.type = "button";
      retryBtn.className = "action-btn";
      retryBtn.textContent = "重试";
      retryBtn.addEventListener("click", () => retryAssistantMessage(row));
      const editBtn = document.createElement("button");
      editBtn.type = "button";
      editBtn.className = "action-btn";
      editBtn.textContent = "编辑";
      editBtn.addEventListener("click", () => startEditAssistantMessage(row));
      actions.appendChild(retryBtn);
      actions.appendChild(editBtn);
    }
    const deleteBtn = document.createElement("button");
    deleteBtn.type = "button";
    deleteBtn.className = "action-btn action-btn-danger";
    deleteBtn.textContent = "删除";
    deleteBtn.addEventListener("click", () => deleteChatMessage(row));
    actions.appendChild(deleteBtn);
    row.appendChild(actions);
    log.appendChild(row);
    log.scrollTop = log.scrollHeight;
    return contentEl;
  }

  function chatMessageIndex(row) {
    if (!row) return -1;
    const log = row.closest?.("#grokChatLog") || row.parentElement;
    return Array.from(log?.querySelectorAll?.(".message-row") || []).indexOf(row);
  }

  function confirmTrailingMessages(action, trailingCount) {
    if (trailingCount <= 0) return true;
    return window.confirm(`${action}会同时删除后续 ${trailingCount} 条消息，是否继续？`);
  }

  function truncateChatBranch(session, keepCount) {
    session.messages = (Array.isArray(session.messages) ? session.messages : []).slice(0, keepCount);
    session.promptCacheKey = createPromptCacheKey();
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
  }

  function deleteChatMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const targetIndex = chatMessageIndex(row);
    if (targetIndex < 0 || targetIndex >= messages.length) return;
    const trailingCount = messages.length - targetIndex - 1;
    const prompt = trailingCount > 0
      ? `删除这条消息会同时删除后续 ${trailingCount} 条消息，是否继续？`
      : "确认删除这条消息？";
    if (!window.confirm(prompt)) return;
    truncateChatBranch(session, targetIndex);
  }

  function startEditChatMessage(row, content, attachment) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const targetIndex = chatMessageIndex(row);
    if (targetIndex < 0) return;

    const bubble = row.querySelector(".message-bubble");
    const actions = row.querySelector(".message-actions");
    if (!bubble || !actions) return;
    bubble.innerHTML = "";
    actions.innerHTML = "";

    const textarea = document.createElement("textarea");
    textarea.className = "edit-msg-input";
    textarea.value = String(content || "");
    const actionWrap = document.createElement("div");
    actionWrap.className = "edit-msg-actions";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.className = "btn btn-primary";
    saveBtn.textContent = "保存";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.className = "btn btn-outline";
    cancelBtn.textContent = "取消";

    const cancel = () => rerenderChatThread();
    saveBtn.addEventListener("click", async () => {
      const next = String(textarea.value || "").trim();
      if (!next) {
        showToast("消息不能为空", "info");
        return;
      }
      const trailingCount = messages.length - targetIndex - 1;
      if (!confirmTrailingMessages("编辑这条消息", trailingCount)) return;
      const edited = { ...messages[targetIndex], content: next };
      if (attachment) edited.attachment = attachment;
      session.messages = messages.slice(0, targetIndex).concat(edited);
      session.promptCacheKey = createPromptCacheKey();
      session.updatedAt = Date.now();
      saveChatSessions();
      renderChatSessions();
      rerenderChatThread();
      const contentEl = appendChatMessage("assistant", "");
      await requestChatCompletion(session, contentEl);
    });
    cancelBtn.addEventListener("click", cancel);
    textarea.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
        event.preventDefault();
        saveBtn.click();
      } else if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    });

    bubble.appendChild(textarea);
    actionWrap.appendChild(saveBtn);
    actionWrap.appendChild(cancelBtn);
    bubble.appendChild(actionWrap);
    textarea.focus();
    textarea.select();
  }

  function startEditAssistantMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const rowIndex = chatMessageIndex(row);
    if (rowIndex < 0 || rowIndex >= messages.length) return;
    const msg = messages[rowIndex];
    if (!msg || msg.role !== "assistant") return;

    const bubble = row.querySelector(".message-bubble");
    const actions = row.querySelector(".message-actions");
    if (!bubble || !actions) return;
    bubble.innerHTML = "";
    actions.classList.add("hidden");

    const textarea = document.createElement("textarea");
    textarea.className = "edit-msg-input";
    textarea.value = String(msg.content || "");
    const actionWrap = document.createElement("div");
    actionWrap.className = "edit-msg-actions";
    const saveBtn = document.createElement("button");
    saveBtn.type = "button";
    saveBtn.className = "btn btn-primary";
    saveBtn.textContent = "保存";
    const cancelBtn = document.createElement("button");
    cancelBtn.type = "button";
    cancelBtn.className = "btn btn-outline";
    cancelBtn.textContent = "取消";
    const cancel = () => rerenderChatThread();
    saveBtn.addEventListener("click", () => {
      const next = String(textarea.value || "").trim();
      if (!next) {
        showToast("消息不能为空", "info");
        return;
      }
      // Editing a historical answer changes the branch's premise, so the
      // descendants that answered the old text must not survive it. Only the
      // edited message itself is kept.
      const trailing = messages.length - (rowIndex + 1);
      if (!confirmTrailingMessages("编辑这条回复", trailing)) {
        return;
      }
      msg.content = next;
      msg.reasoning = "";
      msg.tools = [];
      session.promptCacheKey = createPromptCacheKey();
      session.updatedAt = Date.now();
      if (trailing > 0) {
        session.messages = messages.slice(0, rowIndex + 1);
      }
      saveChatSessions();
      renderChatSessions();
      rerenderChatThread();
    });
    cancelBtn.addEventListener("click", cancel);
    textarea.addEventListener("keydown", (event) => {
      if ((event.metaKey || event.ctrlKey) && event.key === "Enter") {
        event.preventDefault();
        saveBtn.click();
      } else if (event.key === "Escape") {
        event.preventDefault();
        cancel();
      }
    });
    bubble.appendChild(textarea);
    actionWrap.appendChild(saveBtn);
    actionWrap.appendChild(cancelBtn);
    bubble.appendChild(actionWrap);
    textarea.focus();
    textarea.select();
  }

  async function requestChatCompletion(session, contentEl) {
    if (!chatState.modelsLoaded || !chatState.model) {
      updateChatStatus("模型目录尚未加载，无法发送", "error");
      return;
    }
    const requestGeneration = Number(chatState.requestGeneration || 0) + 1;
    chatState.requestGeneration = requestGeneration;
    const abortController = new AbortController();
    const isCurrentRequest = () => chatState.requestGeneration === requestGeneration;
    if (chatState.abortController) chatState.abortController.abort();
    let assistantText = "";
    let answerText = "";
    let reasoningText = "";
    const toolActivities = new Map();
    let saved = false;
    let streamCompleted = false;
    const persistAssistant = () => {
      if (!isCurrentRequest() || saved || (!answerText.trim() && !reasoningText.trim() && !toolActivities.size)) return;
      saved = true;
      session.messages.push({ role: "assistant", content: answerText, reasoning: reasoningText, tools: Array.from(toolActivities.values()) });
      session.updatedAt = Date.now();
    };
    let reasoningOpen = false;
    let hasThink = false;
    let thinkStartAt = null;
    let thinkElapsed = null;
    let thinkAutoCollapsed = false;
    chatState.sending = true;
    chatState.abortController = abortController;
    setChatSendButtonState(true);
    updateChatStatus("连接中...", "connecting");

    const updateAssistantView = () => {
      if (!isCurrentRequest() || !contentEl) return;
      let savedThinkStates = null;
      if (hasThink && thinkAutoCollapsed) {
        const blocks = contentEl.querySelectorAll(".think-block[data-think=\"true\"]");
        if (blocks.length) {
          savedThinkStates = Array.from(blocks).map((b) => b.hasAttribute("open"));
        }
      }
      setRenderedHTML(contentEl, renderAssistantContent(assistantText) + renderToolActivity(Array.from(toolActivities.values())));
      if (hasThink) {
        updateThinkSummary(contentEl, typeof thinkElapsed === "number" ? thinkElapsed : null);
        const blocks = contentEl.querySelectorAll(".think-block[data-think=\"true\"]");
        blocks.forEach((block, index) => {
          if (savedThinkStates && index < savedThinkStates.length) {
            if (savedThinkStates[index]) {
              block.setAttribute("open", "");
            } else {
              block.removeAttribute("open");
            }
          } else if (thinkElapsed === null || thinkElapsed === undefined) {
            block.setAttribute("open", "");
          } else if (!thinkAutoCollapsed) {
            block.removeAttribute("open");
            thinkAutoCollapsed = true;
          }
        });
      }
      applyImageGrid(contentEl);
      enhanceBrokenImages(contentEl);
      if (hasThink) {
        const thinkNodes = contentEl.querySelectorAll(".think-content");
        thinkNodes.forEach((node) => {
          node.scrollTop = node.scrollHeight;
        });
      }
      const log = document.getElementById("grokChatLog");
      if (log) log.scrollTop = log.scrollHeight;
    };
    const scheduleAssistantView = () => {
      if (!isCurrentRequest() || chatState.renderFrame) return;
      chatState.renderFrame = requestAnimationFrame(() => {
        chatState.renderFrame = 0;
        updateAssistantView();
      });
    };

    try {
      const payload = buildResponsesPayload();
      const res = await fetch(`${toolInferencePrefix()}/responses`, {
        method: "POST",
        headers: toolAuthHeaders({ "Content-Type": "application/json" }),
        body: JSON.stringify(payload),
        signal: abortController.signal,
      });
      if (handleUnauthorized(res)) return { aborted: false, text: "" };
      if (!res.ok || !res.body) {
        throw new Error(await res.text());
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      // Line-oriented SSE scanner: tolerates CRLF line endings across chunk
      // boundaries and processes a trailing frame not followed by a blank line.
      // Bare CR line endings are not supported.
      let carry = "";
      const pendingData = [];
      let finishReason = "";
      const finishReasonSweep = (finalFinish) => {
        finishReason = finalFinish;
        streamCompleted = true;
        for (const tool of toolActivities.values()) {
          if (tool.status !== "failed") tool.status = finalFinish === "tool_calls" ? "awaiting_result" : "completed";
        }
      };
      const dispatchData = (data) => {
        if (!isCurrentRequest()) return;
        if (!data || data === "[DONE]") {
          if (data === "[DONE]") streamCompleted = true;
          return;
        }
        let event = null;
        try {
          event = JSON.parse(data);
        } catch (err) {
          throw new Error("服务端返回了无效的流数据");
        }
        if (event?.error) {
          throw new Error(event.error.message || String(event.error));
        }
        const kind = String(event?.type || "");
        if (kind === "response.failed" || kind === "error") {
          throw new Error(event?.response?.error?.message || event?.message || "上游返回失败");
        }
        if (kind === "response.completed" || kind === "response.incomplete") {
          const hasCalls = Array.from(toolActivities.values()).some((tool) => tool.status !== "failed");
          finishReasonSweep(kind === "response.incomplete" ? "length" : hasCalls ? "tool_calls" : "stop");
          scheduleAssistantView();
          return;
        }
        const item = event?.item;
        if (item && (kind === "response.output_item.added" || kind === "response.output_item.done")) {
          const itemType = String(item.type || "");
          const done = kind === "response.output_item.done";
          if (itemType === "web_search_call" || itemType === "x_search_call") {
            const id = String(item.id || item.call_id || itemType);
            const itemStatus = String(item.status || "").toLowerCase();
            const status = itemStatus === "failed" || itemStatus === "incomplete" ? "failed" : done ? "completed" : "in_progress";
            toolActivities.set(id, { id, name: itemType, status, detail: item.action?.query || "" });
          } else if (itemType === "function_call") {
            const id = String(item.call_id || item.id || "tool");
            const previous = toolActivities.get(id) || { id, name: "工具", detail: "", status: "in_progress" };
            if (item.name) previous.name = item.name;
            if (typeof item.arguments === "string" && item.arguments) previous.detail = item.arguments;
            if (done) previous.status = "awaiting_result";
            toolActivities.set(id, previous);
          }
          scheduleAssistantView();
          return;
        }
        if (kind === "response.function_call_arguments.delta" && typeof event.delta === "string") {
          const id = String(event.item_id || "tool");
          const previous = toolActivities.get(id) || { id, name: "工具", detail: "", status: "in_progress" };
          previous.detail += event.delta;
          toolActivities.set(id, previous);
          scheduleAssistantView();
          return;
        }
        const delta = typeof event?.delta === "string" ? event.delta : "";
        if (!delta) return;
        if (kind === "response.reasoning_summary_text.delta" || kind === "response.reasoning_text.delta") {
          reasoningText += delta;
          if (!reasoningOpen) {
            assistantText += "<think>";
            reasoningOpen = true;
          }
          assistantText += delta;
          updateChatStatus("思考中...", "connecting");
        } else if (kind === "response.output_text.delta" || kind === "response.refusal.delta") {
          if (reasoningOpen) {
            assistantText += "</think>";
            reasoningOpen = false;
          }
          answerText += delta;
          assistantText += delta;
          updateChatStatus("生成中...", "connecting");
        } else {
          return;
        }
        if (!hasThink && assistantText.includes("<think>")) {
          hasThink = true;
          thinkStartAt = Date.now();
          thinkElapsed = null;
        }
        if (hasThink && thinkStartAt && thinkElapsed === null && assistantText.includes("</think>")) {
          thinkElapsed = Math.max(1, Math.round((Date.now() - thinkStartAt) / 1000));
        }
        scheduleAssistantView();
      };
      const handleLine = (line) => {
        if (line === "") {
          const data = pendingData.join("");
          pendingData.length = 0;
          dispatchData(data);
          return;
        }
        if (line.startsWith("data:")) {
          pendingData.push(line.slice(5).trimStart());
        }
      };
      const feedChunk = (text) => {
        const parts = (carry + text).split("\n");
        carry = parts.pop();
        for (let line of parts) {
          if (line.endsWith("\r")) line = line.slice(0, -1);
          handleLine(line);
        }
      };
      while (isCurrentRequest()) {
        const { value, done } = await reader.read();
        if (done) break;
        feedChunk(decoder.decode(value, { stream: true }));
      }
      if (!isCurrentRequest()) return { aborted: true, stale: true, text: "" };
      feedChunk(decoder.decode());
      if (carry) handleLine(carry.endsWith("\r") ? carry.slice(0, -1) : carry);
      handleLine("");
      if (!streamCompleted) throw new Error("连接中断，已保留收到的部分回答");
      if (chatState.renderFrame) cancelAnimationFrame(chatState.renderFrame);
      chatState.renderFrame = 0;
      updateAssistantView();
      if (!assistantText.trim() && !toolActivities.size) {
        throw new Error("服务端未返回可显示的内容，请重试或检查上游日志");
      }
      persistAssistant();
      session.updatedAt = Date.now();
      saveChatSessions();
      if (finishReason === "length") {
        updateChatStatus("回复因达到长度上限被截断", "error");
      } else {
        updateChatStatus("完成", "ok");
      }
      return { aborted: false, text: assistantText.trim() };
    } catch (err) {
      if (!isCurrentRequest()) return { aborted: true, stale: true, text: "" };
      persistAssistant();
      if (err && err.name === "AbortError") {
        if (contentEl && !assistantText.trim()) {
          assistantText = "[stopped]";
          updateAssistantView();
        }
        updateChatStatus("已停止", "error");
        return { aborted: true, text: assistantText.trim() };
      }
      if (contentEl) {
        if (!assistantText.trim()) assistantText = `[error] ${err.message || err}`;
        updateAssistantView();
      }
      updateChatStatus(err.message || "发送失败", "error");
      return { aborted: false, text: "" };
    } finally {
      if (isCurrentRequest()) {
        if (chatState.renderFrame) cancelAnimationFrame(chatState.renderFrame);
        chatState.renderFrame = 0;
        chatState.sending = false;
        chatState.abortController = null;
        setChatSendButtonState(false);
        renderChatSessions();
        saveChatSessions();
      }
    }
  }

  async function retryAssistantMessage(row) {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!row || !session) return;
    const messages = Array.isArray(session.messages) ? session.messages : [];
    const rowIndex = chatMessageIndex(row);
    if (rowIndex < 0 || rowIndex >= messages.length) return;
    if (messages[rowIndex]?.role !== "assistant") return;

    let lastUserIndex = -1;
    for (let i = rowIndex - 1; i >= 0; i -= 1) {
      if (messages[i]?.role === "user") {
        lastUserIndex = i;
        break;
      }
    }
    if (lastUserIndex < 0) {
      showToast("没有可重试的上文", "info");
      return;
    }

    const trailingCount = messages.length - rowIndex - 1;
    if (!confirmTrailingMessages("重试这条回复", trailingCount)) return;
    session.messages = messages.slice(0, lastUserIndex + 1);
    session.promptCacheKey = createPromptCacheKey();
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
    const contentEl = appendChatMessage("assistant", "");
    await requestChatCompletion(session, contentEl);
  }

  function rerenderChatThread() {
    const log = document.getElementById("grokChatLog");
    const empty = document.getElementById("grokChatEmpty");
    if (!log) return;
    log.innerHTML = "";
    const session = activeChatSession();
    const messages = Array.isArray(session?.messages) ? session.messages : [];
    if (messages.length === 0) {
      if (empty) {
        empty.style.display = "block";
        log.appendChild(empty);
      }
      return;
    }
    messages.forEach((msg) => appendChatMessage(msg.role, msg.content, msg.attachment, msg));
  }

  function renderChatSessions() {
    const list = document.getElementById("grokSessionList");
    if (!list) return;
    list.innerHTML = "";
    chatState.sessions.forEach((session) => {
      const item = document.createElement("button");
      item.type = "button";
      item.className = `session-item${session.id === chatState.activeId ? " active" : ""}`;
      item.dataset.id = session.id;

      const title = document.createElement("span");
      title.className = "session-title";
      title.textContent = session.title || "新会话";
      title.addEventListener("dblclick", (event) => {
        event.stopPropagation();
        startRenameChatSession(session.id, title);
      });

      const meta = document.createElement("span");
      meta.className = "session-meta";
      meta.textContent = relativeTime(session.updatedAt);

      const delBtn = document.createElement("button");
      delBtn.type = "button";
      delBtn.className = "session-delete";
      delBtn.title = "删除";
      delBtn.textContent = "×";
      delBtn.addEventListener("click", (event) => {
        event.stopPropagation();
        deleteChatSession(session.id);
      });

      item.addEventListener("click", () => {
        switchChatSession(session.id);
        if (isMobileChatSidebar()) closeChatSidebar();
      });

      item.appendChild(title);
      item.appendChild(meta);
      item.appendChild(delBtn);
      list.appendChild(item);
    });
  }

  function syncChatModelUI() {
    const route = chatState.routes?.find((item) => item.id === chatState.model);
    const effortControl = document.getElementById("grokReasoningEffort");
    const session = activeChatSession();
    if (effortControl) {
      const labels = { none: "关闭", low: "低", medium: "中", high: "高", xhigh: "极高" };
      const advertised = Array.isArray(route?.reasoning_efforts)
        ? route.reasoning_efforts.map((value) => String(value || "").trim().toLowerCase()).filter(Boolean)
        : null;
      const efforts = advertised || ["none", "low", "medium", "high", "xhigh"];
      const previous = String(session?.reasoningEffort || effortControl.value || "");
      effortControl.replaceChildren();
      const automatic = document.createElement("option");
      automatic.value = "";
      automatic.textContent = "自动";
      effortControl.appendChild(automatic);
      efforts.forEach((value) => {
        const option = document.createElement("option");
        option.value = value;
        option.textContent = labels[value] || value;
        effortControl.appendChild(option);
      });
      effortControl.disabled = route?.supports_reasoning_effort === false;
      const fallback = efforts.includes(String(route?.default_reasoning_effort || "")) ? String(route.default_reasoning_effort) : "";
      effortControl.value = efforts.includes(previous) ? previous : fallback;
      if (session) session.reasoningEffort = effortControl.value;
    }
    const webSearch = document.getElementById("grokWebSearch");
    if (webSearch) {
      const unsupported = route?.supports_backend_search === false;
      webSearch.disabled = unsupported;
      if (unsupported) {
        webSearch.checked = false;
        if (session) session.webSearch = false;
      }
    }
    const label = document.getElementById("grokModelLabel");
    if (label) label.textContent = chatState.model;
    if (session) {
      session.model = chatState.model;
    }
  }

  function renderChatModelDropdown() {
    const dropdown = document.getElementById("grokModelDropdown");
    if (!dropdown) return;
    dropdown.innerHTML = "";
    chatState.models.forEach((model) => {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = `model-option${model === chatState.model ? " active" : ""}`;
      btn.textContent = model;
      btn.dataset.model = model;
      dropdown.appendChild(btn);
    });
  }

  function preferredBuildModel(models) {
    const list = Array.isArray(models) ? models : [];
    return list[0] || "";
  }

  async function loadChatModels() {
    try {
      let routes;
      if (toolAuthState.mode === "client") {
        const res = await fetch("/v1/models", { headers: toolAuthHeaders() });
        if (!res.ok) throw new Error(`模型目录加载失败 (HTTP ${res.status})`);
        const payload = await res.json();
        routes = (Array.isArray(payload?.data) ? payload.data : [])
          .map((item) => typeof item === "string" ? { id: item } : item)
          .filter((item) => String(item?.id || "").trim())
          .map((item) => {
            const route = { ...item, id: String(item.id).trim() };
            // /v1/models is authoritative when it declares capabilities. Only
            // old servers that omit the field get the compatibility chat route.
            if (!Array.isArray(route.capabilities)) route.capabilities = ["chat"];
            return route;
          });
      } else {
        const catalogPromise = window.GrokModelCatalogPromise || (window.GrokModelCatalogPromise = fetch("/api/grok/models").then(async (res) => {
          if (handleUnauthorized(res)) throw new Error("登录已失效");
          if (!res.ok) throw new Error("模型目录加载失败");
          const payload = await res.json();
          return Array.isArray(payload?.data) ? payload.data : [];
        }));
        routes = await catalogPromise;
      }
      const supports = (item, capability) => {
        const capabilities = Array.isArray(item?.capabilities) ? item.capabilities : [];
        // Match the backend's legacy compatibility rule: an old row with no
        // capability metadata is a conversation route, not an invisible model.
        return capabilities.length === 0 ? capability === "chat" : capabilities.includes(capability);
      };
      chatState.routes = routes;
      chatState.capabilities = { chat: routes.some((item) => supports(item, "chat")) };
      chatState.modelsLoaded = true;
      const models = routes
        .filter((item) => supports(item, "chat"))
        .map((item) => String(item?.id || "").trim())
        .filter(Boolean);
      if (models.length === 0) throw new Error("模型目录为空");
      chatState.models = models;
      if (!models.includes(chatState.model)) {
        chatState.model = preferredBuildModel(models);
      }
    } catch (err) {
      chatState.modelsLoaded = false;
      chatState.models = [];
      chatState.model = "";
      updateChatStatus(`模型目录不可用：${err?.message || "加载失败"}`, "error");
    }
  }

  function ensureChatTitle(session) {
    if (!session || !Array.isArray(session.messages) || session.messages.length === 0) return;
    if (session.isDefaultTitle === false) return;
    const firstUser = session.messages.find((msg) => msg && msg.role === "user" && String(msg.content || "").trim());
    if (!firstUser) return;
    session.title = String(firstUser.content || "").replace(/\s+/g, " ").trim().slice(0, 20) || "新会话";
    session.isDefaultTitle = false;
  }

  function renameChatSession(id, newTitle) {
    const session = chatState.sessions.find((item) => item && item.id === id);
    if (!session) return;
    const trimmed = String(newTitle || "").trim();
    session.title = trimmed || "新会话";
    session.isDefaultTitle = !trimmed;
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
  }

  function deleteChatSession(id) {
    if (chatState.sending) return;
    const idx = chatState.sessions.findIndex((item) => item && item.id === id);
    if (idx < 0) return;
    const target = chatState.sessions[idx];
    const count = Array.isArray(target?.messages) ? target.messages.length : 0;
    // Deleting a conversation is irreversible in the local store, so it needs
    // the same explicit confirmation as clearing one.
    if (!window.confirm(count > 0 ? `确认删除该会话及其 ${count} 条消息？` : "确认删除该会话？")) return;
    chatState.sessions.splice(idx, 1);
    if (chatState.sessions.length === 0) {
      const session = createChatSession();
      chatState.sessions = [session];
      chatState.activeId = session.id;
    } else if (chatState.activeId === id) {
      chatState.activeId = chatState.sessions[Math.max(0, idx - 1)].id;
    }
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
  }

  function startRenameChatSession(sessionId, titleEl) {
    const session = chatState.sessions.find((item) => item && item.id === sessionId);
    if (!session || !titleEl || !titleEl.parentNode) return;
    const input = document.createElement("input");
    input.type = "text";
    input.className = "session-rename-input";
    input.value = String(session.title || "");
    input.maxLength = 40;
    titleEl.replaceWith(input);
    input.focus();
    input.select();
    const commit = () => renameChatSession(sessionId, input.value);
    input.addEventListener("blur", commit);
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        input.blur();
      }
      if (event.key === "Escape") {
        input.value = session.title || "新会话";
        input.blur();
      }
    });
  }

  function switchChatSession(id) {
    if (chatState.sending) return;
    if (!id || id === chatState.activeId) return;
    chatState.activeId = id;
    const session = activeChatSession();
    if (session && session.model) {
      chatState.model = session.model;
    }
    syncChatSessionSettings(session);
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
  }

  function clearCurrentChatSession() {
    if (chatState.sending) return;
    const session = activeChatSession();
    if (!session || !Array.isArray(session.messages) || session.messages.length === 0) return;
    if (!window.confirm(`确认清空当前会话的 ${session.messages.length} 条消息？`)) return;
    session.messages = [];
    session.promptCacheKey = createPromptCacheKey();
    session.updatedAt = Date.now();
    saveChatSessions();
    renderChatSessions();
    rerenderChatThread();
    updateChatStatus("当前会话已清空", "ok");
  }

  function newChatSession() {
    if (chatState.sending) return;
    const session = createChatSession();
    chatState.sessions.unshift(session);
    chatState.activeId = session.id;
    syncChatSessionSettings(session);
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    saveChatSessions();
    updateChatStatus("就绪");
  }

  function buildResponsesPayload() {
    const session = activeChatSession();
    if (!session) {
      throw new Error("missing chat session");
    }
    const input = [];
    session.messages.forEach((msg) => {
      const role = msg.role === "assistant" ? "assistant" : "user";
      const parts = [];
      if (msg.content) {
        parts.push({ type: role === "assistant" ? "output_text" : "input_text", text: String(msg.content || "") });
      }
      if (role === "user" && msg.attachment?.dataUrl) {
        parts.push({ type: "input_file", file: { data: msg.attachment.dataUrl } });
      }
      if (parts.length === 0) return;
      input.push({ type: "message", role, content: parts });
    });
    const payload = {
      model: chatState.model,
      input,
      stream: true,
      store: false,
    };
    const systemPrompt = String(document.getElementById("grokSystemInput")?.value || "").trim();
    if (systemPrompt) payload.instructions = systemPrompt;
    const route = chatState.routes?.find((item) => item.id === chatState.model);
    const effort = String(session.reasoningEffort || "").trim();
    const reasoning = {};
    if (effort) reasoning.effort = effort;
    // Every reasoning request is paired with a summary; only "none" must not ask for one.
    if (effort !== "none") reasoning.summary = "auto";
    if (Object.keys(reasoning).length) payload.reasoning = reasoning;
    if (session.promptCacheKey) payload.prompt_cache_key = session.promptCacheKey;
    const tools = [];
    if (session.webSearch && route?.supports_backend_search !== false) tools.push({ type: "web_search" });
    if (session.xSearch) tools.push({ type: "x_search" });
    if (tools.length) payload.tools = tools;
    return payload;
  }

  function syncChatSessionSettings(session) {
    const effort = document.getElementById("grokReasoningEffort");
    const webSearch = document.getElementById("grokWebSearch");
    const xSearch = document.getElementById("grokXSearch");
    if (effort) effort.value = String(session?.reasoningEffort || "");
    if (webSearch) webSearch.checked = session?.webSearch === true;
    if (xSearch) xSearch.checked = session?.xSearch === true;
  }

  async function sendChatMessage() {
    if (chatState.sending) return;
    const input = document.getElementById("grokPromptInput");
    const prompt = String(input?.value || "").trim();
    if (!prompt) return;
    const session = activeChatSession();
    if (!session) return;
    session.messages.push({ role: "user", content: prompt });
    session.updatedAt = Date.now();
    ensureChatTitle(session);
    renderChatSessions();
    rerenderChatThread();
    if (input) {
      input.value = "";
      input.style.height = "40px";
    }

    const contentEl = appendChatMessage("assistant", "");
    await requestChatCompletion(session, contentEl);
  }

  function bindChatEvents() {
    const newBtn = document.getElementById("grokChatNewBtn");
    const clearBtn = document.getElementById("grokChatClearBtn");
    const sendBtn = document.getElementById("grokSendBtn");
    const input = document.getElementById("grokPromptInput");
    const modelChip = document.getElementById("grokModelChip");
    const modelDropdown = document.getElementById("grokModelDropdown");
    const settingsToggle = document.getElementById("grokSettingsToggle");
    const settingsPanel = document.getElementById("grokSettingsPanel");
    const settingsCloseBtn = document.getElementById("grokSettingsCloseBtn");
    const sidebarToggle = document.getElementById("grokChatSidebarToggle");
    const sidebarOverlay = document.getElementById("grokChatSidebarOverlay");
    const collapseBtn = document.getElementById("grokChatCollapseBtn");
    const expandBtn = document.getElementById("grokChatExpandBtn");
    const reasoningEffort = document.getElementById("grokReasoningEffort");
    const webSearch = document.getElementById("grokWebSearch");
    const xSearch = document.getElementById("grokXSearch");

    if (newBtn) newBtn.addEventListener("click", () => {
      newChatSession();
      if (isMobileChatSidebar()) {
        closeChatSidebar();
      }
    });
    if (clearBtn) clearBtn.addEventListener("click", clearCurrentChatSession);
    if (sendBtn) {
      sendBtn.addEventListener("click", () => {
        if (chatState.sending && chatState.abortController) {
          chatState.abortController.abort();
          return;
        }
        sendChatMessage().catch(() => {});
      });
    }
    if (input) {
      let composing = false;
      input.addEventListener("compositionstart", () => {
        composing = true;
      });
      input.addEventListener("compositionend", () => {
        composing = false;
      });
      input.addEventListener("input", () => {
        input.style.height = "40px";
        input.style.height = `${Math.min(input.scrollHeight, 160)}px`;
      });
      input.addEventListener("keydown", (event) => {
        if (event.key === "Enter" && !event.shiftKey) {
          if (composing || event.isComposing) return;
          event.preventDefault();
          sendChatMessage().catch(() => {});
        }
      });
    }
    if (sidebarToggle) sidebarToggle.addEventListener("click", () => toggleChatSidebar());
    if (expandBtn) expandBtn.addEventListener("click", () => openChatSidebar());
    if (collapseBtn) collapseBtn.addEventListener("click", () => closeChatSidebar());
    if (sidebarOverlay) sidebarOverlay.addEventListener("click", () => closeChatSidebar());
    window.addEventListener("resize", syncChatSidebarState);
    if (modelChip && modelDropdown) {
      modelChip.addEventListener("click", (event) => {
        event.stopPropagation();
        closeChatSettingsPanel(settingsToggle, settingsPanel);
        modelDropdown.classList.toggle("show");
      });
      modelDropdown.addEventListener("click", (event) => {
        const btn = event.target.closest(".model-option");
        if (!btn || !modelDropdown.contains(btn)) return;
        chatState.model = String(btn.dataset.model || chatState.model);
        const session = activeChatSession();
        if (session) {
          session.model = chatState.model;
          session.promptCacheKey = createPromptCacheKey();
        }
        syncChatModelUI();
        renderChatModelDropdown();
        saveChatSessions();
        modelDropdown.classList.remove("show");
      });
    }
    if (settingsToggle && settingsPanel) {
      settingsToggle.addEventListener("click", (event) => {
        event.stopPropagation();
        modelDropdown?.classList.remove("show");
        toggleChatSettingsPanel(settingsToggle, settingsPanel);
      });
      settingsPanel.addEventListener("click", (event) => {
        event.stopPropagation();
      });
    }
    if (settingsCloseBtn) {
      settingsCloseBtn.addEventListener("click", (event) => {
        event.stopPropagation();
        closeChatSettingsPanel(settingsToggle, settingsPanel);
        settingsToggle?.focus();
      });
    }
    document.addEventListener("click", () => {
      modelDropdown?.classList.remove("show");
      closeChatSettingsPanel(settingsToggle, settingsPanel);
    });
    document.addEventListener("keydown", (event) => {
      if (event.key !== "Escape") return;
      modelDropdown?.classList.remove("show");
      closeChatSettingsPanel(settingsToggle, settingsPanel);
    });
    [reasoningEffort, webSearch, xSearch].forEach((control) => {
      if (!control) return;
      control.addEventListener("change", () => {
        const session = activeChatSession();
        if (!session) return;
        session.reasoningEffort = String(reasoningEffort?.value || "");
        session.webSearch = webSearch?.checked === true;
        session.xSearch = xSearch?.checked === true;
        session.updatedAt = Date.now();
        saveChatSessions();
      });
    });
    const systemInput = document.getElementById("grokSystemInput");
    if (systemInput) {
      systemInput.addEventListener("input", () => {
        saveGrokToolsUIState({ chatSystemPrompt: String(systemInput.value || "") });
      });
    }
  }

  function toggleChatSettingsPanel(toggle, panel) {
    if (!panel) return;
    const willShow = !panel.classList.contains("show");
    panel.classList.toggle("show", willShow);
    panel.setAttribute("aria-hidden", willShow ? "false" : "true");
    if (toggle) {
      toggle.setAttribute("aria-expanded", willShow ? "true" : "false");
    }
  }

  function closeChatSettingsPanel(toggle, panel) {
    if (!panel) return;
    panel.classList.remove("show");
    panel.setAttribute("aria-hidden", "true");
    if (toggle) {
      toggle.setAttribute("aria-expanded", "false");
    }
  }

  async function applyToolRequestMode() {
    const mode = document.getElementById("grokRequestMode");
    const keyInput = document.getElementById("grokClientKey");
    const status = document.getElementById("grokClientKeyStatus");
    const nextMode = mode?.value === "client" ? "client" : "admin";
    const nextKey = nextMode === "client" ? String(keyInput?.value || "").trim() : "";
    if (nextMode === "client" && !nextKey) {
      if (status) status.textContent = "请输入 Client Key";
      return;
    }
    persistChatSessionsNow();
    if (chatState.abortController) chatState.abortController.abort();
    chatState.requestGeneration += 1;
    toolAuthState.mode = nextMode;
    toolAuthState.apiKey = nextKey;
    chatState.sessions = [];
    chatState.activeId = "";
    chatState.model = "";
    chatState.models = [];
    chatState.modelsLoaded = false;
    await loadChatModels();
    loadChatSessions();
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    if (status) status.textContent = nextMode === "client" ? "Client Key 已应用（仅当前标签页）" : "管理员会话";
  }

  function bindToolRequestMode() {
    const mode = document.getElementById("grokRequestMode");
    const keyInput = document.getElementById("grokClientKey");
    const apply = document.getElementById("grokClientKeyApply");
    if (!mode) return;
    const sync = () => {
      const client = mode.value === "client";
      keyInput?.classList.toggle("hidden", !client);
      apply?.classList.toggle("hidden", !client);
    };
    mode.addEventListener("change", () => {
      sync();
      if (mode.value !== "client") applyToolRequestMode().catch(() => {});
    });
    apply?.addEventListener("click", () => applyToolRequestMode().catch(() => {}));
    keyInput?.addEventListener("keydown", (event) => {
      if (event.key === "Enter") {
        event.preventDefault();
        applyToolRequestMode().catch(() => {});
      }
    });
    sync();
  }

  async function initChat() {
    bindToolRequestMode();
    await loadChatModels();
    loadChatSessions();
    const uiState = loadGrokToolsUIState();
    if (!chatState.models.includes(chatState.model)) {
      chatState.model = preferredBuildModel(chatState.models);
    }
    const systemInput = document.getElementById("grokSystemInput");
    if (systemInput && typeof uiState.chatSystemPrompt === "string") {
      systemInput.value = uiState.chatSystemPrompt;
    }
    syncChatSessionSettings(activeChatSession());
    syncChatModelUI();
    renderChatModelDropdown();
    renderChatSessions();
    rerenderChatThread();
    setChatSendButtonState(false);
    bindChatEvents();
    closeChatSidebar();
    syncChatSidebarState();
  }


  async function init() {
    await initChat();
    window.addEventListener("beforeunload", persistChatSessionsNow);
  }
  document.addEventListener("DOMContentLoaded", init);
})();
