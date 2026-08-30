// Shared modal primitives: a focus trap, an app-background inert toggle, and a
// generic confirm dialog. Kept in one place so every dialog traps focus, hides
// the background from assistive tech, and behaves consistently.
import { elem } from "./ui.js";
// Monotonic counter giving each confirmDialog invocation unique element ids, so
// two dialogs cannot collide on aria-labelledby/aria-describedby targets.
let dialogSeq = 0;
// Reference count of open modals so nested/overlapping modals compose: the
// background is inerted while depth > 0 and only un-inerted when the last modal
// closes.
let inertDepth = 0;
const FOCUSABLE = 'button:not([disabled]), [href], input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])';
// trapFocus keeps Tab and Shift+Tab cycling within container until released.
export function trapFocus(container) {
    const handler = (e) => {
        if (e.key !== "Tab")
            return;
        const items = Array.from(container.querySelectorAll(FOCUSABLE)).filter((el) => !el.hidden && el.offsetParent !== null);
        if (items.length === 0) {
            e.preventDefault();
            return;
        }
        const first = items[0];
        const last = items[items.length - 1];
        const active = document.activeElement;
        if (!active || items.indexOf(active) === -1) {
            // Focus escaped the focusable set: pull it back rather than let Tab leave.
            e.preventDefault();
            (e.shiftKey ? last : first).focus();
        }
        else if (e.shiftKey && active === first) {
            e.preventDefault();
            last.focus();
        }
        else if (!e.shiftKey && active === last) {
            e.preventDefault();
            first.focus();
        }
    };
    container.addEventListener("keydown", handler);
    return () => container.removeEventListener("keydown", handler);
}
// setAppInert marks the main application container inert and aria-hidden (or
// clears it) while a modal is open. Only .app-container is toggled: the modal
// overlays and the toast container are its siblings, so inerting the whole body
// would trap the very dialog we are showing. It is reference-counted: each
// on() increments the depth and each off() decrements it (floored at 0), so a
// nested inner modal closing does not un-inert the background while an outer
// modal is still open. The background is inert while depth > 0 and cleared only
// when the last modal closes.
export function setAppInert(on) {
    const app = document.querySelector(".app-container");
    if (!app)
        return;
    inertDepth = on ? inertDepth + 1 : Math.max(0, inertDepth - 1);
    if (inertDepth > 0) {
        app.setAttribute("inert", "");
        app.setAttribute("aria-hidden", "true");
    }
    else {
        app.removeAttribute("inert");
        app.removeAttribute("aria-hidden");
    }
}
// confirmDialog shows a modal asking the user to confirm an action. It defaults
// to Cancel (focused), closes on Escape or a backdrop click, traps focus,
// inerts the background, and restores focus to the trigger. Resolves true only
// if the user confirms.
export function confirmDialog(opts) {
    return new Promise((resolve) => {
        const prevFocus = document.activeElement;
        const uid = ++dialogSeq;
        const titleId = `confirm-title-${uid}`;
        const descId = `confirm-desc-${uid}`;
        const overlay = elem("div", "modal-overlay open");
        overlay.setAttribute("role", "dialog");
        overlay.setAttribute("aria-modal", "true");
        overlay.setAttribute("aria-labelledby", titleId);
        overlay.setAttribute("aria-describedby", descId);
        const card = elem("div", "modal-card");
        const title = elem("h3", undefined, opts.title);
        title.id = titleId;
        title.style.cssText = "font-size:16px;font-weight:700;color:var(--text-primary);margin-bottom:6px;";
        const body = elem("p", undefined, opts.body);
        body.id = descId;
        body.style.cssText = "font-size:12px;color:var(--text-secondary);line-height:1.5;";
        const actions = elem("div", "settings-actions");
        const cancel = elem("button", "btn btn-secondary", opts.cancelLabel ?? "Cancel");
        cancel.setAttribute("type", "button");
        const confirm = elem("button", `btn ${opts.danger ? "btn-danger" : "btn-primary"}`, opts.confirmLabel ?? "Confirm");
        confirm.setAttribute("type", "button");
        actions.append(cancel, confirm);
        card.append(title, body, actions);
        overlay.appendChild(card);
        const release = trapFocus(overlay);
        const onKey = (e) => {
            if (e.key === "Escape")
                close(false);
        };
        function close(result) {
            release();
            document.removeEventListener("keydown", onKey);
            overlay.remove();
            setAppInert(false);
            prevFocus?.focus();
            resolve(result);
        }
        cancel.addEventListener("click", () => close(false));
        confirm.addEventListener("click", () => close(true));
        overlay.addEventListener("click", (e) => {
            if (e.target === overlay)
                close(false);
        });
        document.addEventListener("keydown", onKey);
        setAppInert(true);
        document.body.appendChild(overlay);
        cancel.focus();
    });
}
