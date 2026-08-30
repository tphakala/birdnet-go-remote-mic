export function showToast(message, type = "info", durationMs = 3200) {
    const container = document.getElementById("toast-root");
    if (!container)
        return;
    const toast = document.createElement("div");
    toast.className = `toast toast-${type}`;
    let iconSvg = "";
    if (type === "warn") {
        iconSvg = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="m21.73 18-8-14a2 2 0 0 0-3.48 0l-8 14A2 2 0 0 0 4 21h16a2 2 0 0 0 1.73-3Z"></path><line x1="12" y1="9" x2="12" y2="13"></line><line x1="12" y1="17" x2="12.01" y2="17"></line></svg>`;
    }
    else if (type === "error") {
        iconSvg = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"></circle><line x1="15" y1="9" x2="9" y2="15"></line><line x1="9" y1="9" x2="15" y2="15"></line></svg>`;
    }
    else {
        iconSvg = `<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"></path><polyline points="22 4 12 14.01 9 11.01"></polyline></svg>`;
    }
    const icon = document.createElement("span");
    icon.className = "toast-icon";
    icon.innerHTML = iconSvg; // static, trusted markup
    const msg = document.createElement("span");
    msg.className = "toast-msg";
    msg.textContent = message;
    toast.append(icon, msg);
    container.appendChild(toast);
    window.setTimeout(() => {
        toast.style.animation = "toast-out 0.25s cubic-bezier(0.16, 1, 0.3, 1) forwards";
        window.setTimeout(() => {
            toast.remove();
        }, 250);
    }, durationMs);
}
