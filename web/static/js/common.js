// Common JavaScript functions

// Show toast notification
function showToast(msg, type = 'success') {
  const container = document.getElementById("toastContainer") || document.body;
  const toast = document.createElement("div");
  toast.className = "toast";
  const color = type === 'success' ? '#34d399' : type === 'info' ? '#38bdf8' : '#fb7185';
  const icon = type === 'success' ? '✅' : type === 'info' ? 'ℹ️' : '❌';
  toast.style.borderLeft = `4px solid ${color}`;
  const iconSpan = document.createElement("span");
  iconSpan.textContent = icon;
  toast.appendChild(iconSpan);
  toast.appendChild(document.createTextNode(` ${msg}`));
  container.appendChild(toast);
  requestAnimationFrame(() => toast.classList.add("show"));
  setTimeout(() => {
    toast.classList.remove("show");
    setTimeout(() => toast.remove(), 400);
  }, 2800);
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
// Update Sidebar Usage Stats
async function updateSidebarUsage() {
  const footerUsage = document.getElementById("footerUsageText");

  if (!footerUsage) return;

  try {
    const res = await fetch("/api/accounts");
    if (!res.ok) return; // Silent fail if unauthorized or error
    const accounts = await res.json();

    const totalUsage = accounts.reduce((sum, acc) => sum + (acc.usage_daily || 0), 0);
    footerUsage.textContent = `${Math.floor(totalUsage)}`;
  } catch (e) {
    console.error("Failed to update sidebar usage", e);
  }
}

// Initialize
document.addEventListener('DOMContentLoaded', () => {
  updateSidebarUsage();
});
