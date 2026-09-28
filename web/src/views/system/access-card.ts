import { api, apiErrorMessage, firstProblem, isRefusal } from "../../lib/api.ts";
import { store } from "../../lib/store.ts";
import { clearBusy, copyText, part, scrollBehavior, setBusy, setButtonLabel, setFieldError, setText, showUnconfirmed, svgIcon } from "../../lib/ui.ts";
import { confirmDialog } from "../../lib/modal.ts";
import { sentence } from "../../lib/text.ts";
import { generateToken, setToken } from "../../lib/auth.ts";
import { showToast } from "../../components/toast.ts";
import type { Config } from "../../lib/types.ts";

// Access-token reveal toggle glyphs, swapped by setReveal. 12px to sit on the
// .btn control beside the token field, matching the copy button's glyph size.
const ICON_EYE =
  svgIcon('<path d="M1 12s4-8 11-8 11 8 11 8-4 8-11 8-11-8-11-8z"></path><circle cx="12" cy="12" r="3"></circle>', 12);
const ICON_EYE_OFF =
  svgIcon('<path d="M17.94 17.94A10.07 10.07 0 0 1 12 20c-7 0-11-8-11-8a18.45 18.45 0 0 1 5.06-5.94M9.9 4.24A9.12 9.12 0 0 1 12 4c7 0 11 8 11 8a18.5 18.5 0 0 1-2.16 3.19m-6.72-1.07a3 3 0 1 1-4.24-4.24"></path><line x1="1" y1="1" x2="23" y2="23"></line>', 12);

// TOKEN_RULE mirrors the appliance's auth.token validation (auth.ValidToken)
// so an obviously invalid token is caught before the round trip.
const TOKEN_RULE = /^(|[A-Za-z0-9._~-]{12,128})$/;

// AccessCard is the Access Control card (#sys-auth-card): the shared access
// token with reveal, copy, generate, and the save and discard flow.
export class AccessCard {
  private readonly cardEl: HTMLElement | null;
  private readonly stateEl: HTMLElement | null;
  private readonly tokenEl: HTMLInputElement | null;
  private readonly errorEl: HTMLElement | null;
  private readonly actionsEl: HTMLElement | null;
  private dirty = false;
  private saving = false;

  constructor(root: HTMLElement | null) {
    this.cardEl = root;
    this.stateEl = part(root, "sys-auth-state");
    this.tokenEl = part<HTMLInputElement>(root, "sys-auth-token");
    this.errorEl = part(root, "sys-auth-error");
    this.actionsEl = part(root, "sys-auth-actions");
    this.bind();
  }

  // config fills the card from a config read, unless it holds unsaved edits.
  public config(cfg: Config): void {
    if (!this.dirty) this.populate(cfg);
  }

  // focusToken scrolls the card into view and moves focus to the token field.
  // Used by the open-access banner link so following it lands on the control
  // that resolves the warning.
  public focusToken(): void {
    if (!this.cardEl || this.cardEl.hidden) return;
    this.cardEl.scrollIntoView({ behavior: scrollBehavior(), block: "start" });
    // preventScroll: the scroll above already positions the card; a focus
    // scroll would fight a smooth one with an instant jump.
    this.tokenEl?.focus({ preventScroll: true });
  }

  // setReveal shows or hides the token field and keeps the reveal button's
  // label and accessible name in step. It is the single source of the reveal
  // state, used by the reveal toggle, Generate (reveals), and a successful save
  // (re-hides), so the state is never written in two places that could diverge.
  private setReveal(show: boolean): void {
    if (this.tokenEl) this.tokenEl.type = show ? "text" : "password";
    const reveal = part(this.cardEl, "btn-auth-reveal");
    if (reveal) {
      // The visible label and the accessible name both swap Show/Hide; there is
      // no aria-pressed, so the state is carried by the label rather than by a
      // pressed toggle contradicting a changing label.
      setButtonLabel(reveal, show ? "Hide" : "Show");
      const icon = reveal.querySelector<HTMLElement>(".btn-icon");
      if (icon) icon.innerHTML = show ? ICON_EYE_OFF : ICON_EYE; // trusted static markup
      reveal.setAttribute("aria-label", show ? "Hide access token" : "Show access token");
    }
  }

  private bind(): void {
    const input = this.tokenEl;
    if (!input) return;
    input.addEventListener("input", () => {
      this.dirty = true;
      this.setError("");
      if (this.actionsEl) this.actionsEl.hidden = false;
    });
    // The card is not a <form>, so Enter in the token field would do nothing.
    // Wire it to Save, matching the muscle memory of a single-field form.
    input.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        void this.save();
      }
    });
    part(this.cardEl, "btn-auth-reveal")?.addEventListener("click", () => {
      this.setReveal(input.type === "password");
    });
    part(this.cardEl, "btn-auth-copy")?.addEventListener("click", () => {
      const value = input.value.trim();
      if (!value) {
        // An empty field the operator just cleared is not (yet) open access; only
        // a saved empty token is. Distinguish the two so the message is accurate.
        showToast(this.dirty
          ? "Nothing to copy: the token field is empty. Save to switch to open access."
          : "No token to copy: the appliance is on open access.", "warn");
        return;
      }
      // Flag an unsaved value: Generate hands out a token before it is saved,
      // and copying it into BirdNET-Go before saving here would lock players out.
      copyText(value, this.dirty
        ? "Access token copied. It is not saved yet: save it here before the players use it."
        : "Access token copied.");
    });
    part(this.cardEl, "btn-auth-generate")?.addEventListener("click", () => {
      input.value = generateToken();
      // Reveal the generated value: the operator needs to see it to copy it into
      // BirdNET-Go, and a masked random string cannot be verified by eye.
      this.setReveal(true);
      input.dispatchEvent(new Event("input"));
      input.focus();
    });
    part(this.cardEl, "btn-auth-save")?.addEventListener("click", () => void this.save());
    part(this.cardEl, "btn-auth-discard")?.addEventListener("click", () => void this.discard());
  }

  private setError(message: string): void {
    setFieldError(this.tokenEl?.closest(".form-field") ?? null, this.tokenEl, this.errorEl, message);
  }

  private populate(cfg: Config): void {
    if (this.cardEl) this.cardEl.hidden = false;
    const token = cfg.auth?.token ?? "";
    // Only write when changed: this runs on every 3 s config poll (while not
    // dirty) and a redundant assignment to a field the operator has revealed is
    // needless churn.
    if (this.tokenEl && this.tokenEl.value !== token) this.tokenEl.value = token;
    if (this.stateEl) {
      const stateText = token
        ? "Token required: the API, this UI and the RTSP streams ask for credentials."
        : "Open access: anyone on the network can listen and change settings.";
      // #sys-auth-state is a role=status region rewritten on every config event;
      // setText only writes when the text actually changes so a steady state is
      // not re-announced to screen readers each poll.
      setText(this.stateEl, stateText);
      this.stateEl.classList.toggle("locked", !!token);
      this.stateEl.classList.toggle("open", !token);
    }
    this.setError("");
    this.dirty = false;
    if (this.actionsEl) this.actionsEl.hidden = true;
  }

  // discard reverts the token field to the saved value, confirming first when
  // there are unsaved edits so a stray click cannot drop a generated token.
  private async discard(): Promise<void> {
    if (this.dirty) {
      const ok = await confirmDialog({
        title: "Discard changes?",
        body: "The access token has unsaved changes that will be lost.",
        confirmLabel: "Discard",
        danger: true,
      });
      if (!ok) return;
    }
    const cfg = store.getState().config;
    if (cfg) this.populate(cfg);
    // Re-mask the token: Generate reveals it as plaintext, and discarding must not
    // leave the restored saved secret on screen.
    this.setReveal(false);
    // populate hid the actions bar holding the Discard button focus was on,
    // dropping it to <body>; return focus to the token field, matching save.
    this.tokenEl?.focus();
  }

  // save persists the token. Clearing it opens the appliance to the network,
  // so that path confirms first. On success the UI's own stored token is swapped
  // synchronously before any follow-up request, so the next poll already carries
  // the new value and cannot be rejected by the freshly rotated appliance.
  private async save(): Promise<void> {
    if (this.saving || !this.tokenEl) return;
    const token = this.tokenEl.value.trim();
    if (!TOKEN_RULE.test(token)) {
      this.setError("Use 12 to 128 characters: letters, digits, and . _ ~ - only.");
      this.tokenEl.focus();
      return;
    }
    if (!token) {
      const ok = await confirmDialog({
        title: "Allow open access?",
        body: "Removing the token lets anyone on the network listen to the microphones and change settings.",
        confirmLabel: "Allow open access",
        danger: true,
      });
      if (!ok) return;
    }
    const saveBtn = part<HTMLButtonElement>(this.cardEl, "btn-auth-save");
    const discardBtn = part<HTMLButtonElement>(this.cardEl, "btn-auth-discard");
    this.saving = true;
    // Busy affordance that keeps Save focusable (aria-disabled, not disabled), so
    // it does not steal keyboard focus; the saving guard blocks re-entry.
    if (saveBtn) setBusy(saveBtn, "Saving...");
    if (discardBtn) discardBtn.disabled = true;
    // Open the store's rotation window so an in-flight poll rejected while the
    // appliance is switching tokens (it enforces the new one before the PATCH
    // response is fully written) does not pop the login prompt over a working
    // page. It is closed in the finally, after setToken has run.
    store.beginTokenSwap();
    try {
      const res = await api.patchConfig({ auth: { token } });
      this.dirty = false;
      store.applyConfig(res.config);
      // The appliance enforces a patched token immediately, before the reload:
      // mgmtserver PatchConfig calls guard.Set(token) unconditionally right
      // after persisting and BEFORE invoking the reloader, so the new token is
      // live regardless of the outcome. restartRequired is computed only from
      // whether the reloader succeeded; it says nothing about the token, only
      // that the rest of the reload did not take effect. So adopt the new token
      // on both branches, before anything else runs (see setToken): skipping it
      // would leave the UI holding a credential the appliance no longer accepts.
      setToken(token || null);
      // applyConfig above already seeded the authoritative config, so only
      // refreshStatus is needed (it carries authRequired).
      await store.refreshStatus();
      if (res.restartRequired) {
        // The token is already live; it is the rest of the configuration that
        // needs a restart before it takes effect.
        showToast(token
          ? "Access token saved and active. Restart the appliance to finish applying the configuration."
          : "Open access saved and active. Restart the appliance to finish applying the configuration.", "warn");
      } else {
        showToast(token ? "Access token saved. BirdNET-Go and players now need it." : "Open access enabled.", token ? "info" : "warn");
      }
      // Re-hide the token after a successful save: it may have been revealed to
      // copy it, and leaving a saved secret in plain sight is needless exposure.
      this.setReveal(false);
    } catch (err: unknown) {
      const problem = firstProblem(err);
      if (problem) {
        this.setError(sentence(problem.reason));
      } else if (isRefusal(err)) {
        // The appliance refused before applying the token
        // (internal/mgmtserver/config.go:159-166 returns before guard.Set at
        // :190), so the old token is still in force.
        showToast(`Token change failed: ${apiErrorMessage(err)}. The current token is unchanged.`, "error");
      } else {
        // No answer is ambiguous: the appliance applies the token BEFORE it
        // finishes writing the PATCH response, so the new credential may
        // already be in force. Warn rather than imply nothing changed.
        showUnconfirmed("the token change", "the new token may already be in force; if this page locks you out, reload and sign in with it");
      }
    } finally {
      store.endTokenSwap();
      this.saving = false;
      if (saveBtn) clearBusy(saveBtn, "Save Token");
      if (discardBtn) discardBtn.disabled = false;
      // Disabling the Save button the user just activated dropped keyboard focus
      // to <body>; re-enabling does not restore it. After a successful save the
      // actions bar is hidden, so the token input is the sensible landing spot in
      // every case. Restore focus explicitly, matching the convention the toggle
      // and settings paths in dashboard.ts already follow.
      this.tokenEl?.focus();
    }
  }
}
