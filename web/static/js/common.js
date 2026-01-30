// Common JavaScript functions

// Show toast notification
function showToast(msg, type = 'success') {
  const container = document.getElementById("toastContainer");
  const toast = document.createElement("div");
  toast.className = `toast`;
  const color = type === 'success' ? '#34d399' : type === 'info' ? '#38bdf8' : '#fb7185';
  const icon = type === 'success' ? '✅' : type === 'info' ? 'ℹ️' : '❌';
  toast.style.borderLeft = `4px solid ${color}`;
  toast.innerHTML = `<span>${icon}</span> ${msg}`;
  container.appendChild(toast);
  setTimeout(() => toast.remove(), 3000);
}

// Copy text to clipboard
async function copyToClipboard(text) {
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
    } else {
      const el = document.createElement('textarea');
      el.value = text;
      document.body.appendChild(el);
      el.select();
      document.execCommand('copy');
      document.body.removeChild(el);
    }
    showToast("已复制到剪切板");
  } catch (err) {
    showToast("复制失败", "error");
  }
}

// Logout function
async function logout() {
  if (confirm("确定要退出登录吗？")) {
    try {
      await fetch("/api/logout", { method: "POST" });
      window.location.href = "./login.html";
    } catch (err) {
      window.location.href = "./login.html";
    }
  }
}

// Switch between tabs (placeholder - will be implemented with proper routing)
function switchTab(tabName, skipSidebar = false) {
  // This will be replaced with proper client-side routing or page reloads
  console.log("Switching to tab:", tabName);

  // For now, just update the URL with query parameter
  const url = new URL(window.location);
  url.searchParams.set('tab', tabName);
  window.location.href = url.toString();
}
