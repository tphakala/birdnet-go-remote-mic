import { api, ApiError } from "../lib/api.js";
import { store } from "../lib/store.js";
import { apiErrorMessage, clearBusy, deviceStateBadge, elem, formatUptime, modeLabel, renderLoadError, setBusy, setFieldError, setHidden, setText } from "../lib/ui.js";
import { confirmDialog } from "../lib/modal.js";
import { triggerApplianceRestart } from "../components/restart-modal.js";
import { showToast } from "../components/toast.js";
import { generateToken, setToken } from "../lib/auth.js";
import { NOTIFY_FIELDS, buildNotificationsPatch, fieldForServerPath, unparsedThresholds, } from "../lib/notification-settings-core.js";
// OVERRIDE_LABELS maps a serve-override's dotted config field to an operator-
// facing label. An unmapped field falls back to its dotted path.
const OVERRIDE_LABELS = {
    listen: "RTSP listen address",
    "management.listen": "Management listen address",
    "management.certDir": "Certificate directory",
    "management.enabled": "Management API",
    "discovery.enabled": "mDNS discovery",
};
// formatCertTime renders an RFC 3339 timestamp in the operator's locale, falling
// back to the raw string if it does not parse.
function formatCertTime(iso) {
    const d = new Date(iso);
    return Number.isNaN(d.getTime()) ? iso : d.toLocaleString();
}
// TOKEN_RULE mirrors the appliance's auth.token validation (auth.ValidToken)
// so an obviously invalid token is caught before the round trip.
const TOKEN_RULE = /^(|[A-Za-z0-9._~-]{12,128})$/;
export class SystemView {
    tilesEl;
    infoEl;
    infoCardEl;
    rowsEl;
    system = null;
    status = null;
    // Stable per-poll nodes for the diffed telemetry renders: resource tiles keyed
    // by tile key, System-Information rows keyed by label, and Stream-Status rows
    // keyed by ALSA device id. Built once, updated in place, added/removed on change.
    tileEls = new Map();
    infoRows = new Map();
    deviceRows = new Map();
    deviceEmptyRow = null;
    netCardEl;
    netActionsEl;
    discoveryEl;
    netDirty = false;
    overridesEl;
    // Signature of the override set last rendered into the #sys-overrides live
    // region, so a 3s status poll that changes nothing does not re-announce it.
    lastOverridesSig = null;
    certCardEl;
    certInfoEl;
    certFingerprintEl;
    // cert is the management certificate metadata, fetched once (it is immutable
    // for the process lifetime). certPending guards concurrent loads; certUnavailable
    // is set on a 501 so a permanently-unmounted endpoint is not polled forever.
    cert = null;
    certPending = false;
    certUnavailable = false;
    authCardEl;
    authStateEl;
    authTokenEl;
    authErrorEl;
    authActionsEl;
    authDirty = false;
    authSaving = false;
    notifyCardEl;
    notifyActionsEl;
    notifyEnabledEl;
    notifyErrorEl;
    // Threshold inputs and their .form-field wrappers, keyed by NotifyFieldSpec.key,
    // built once in bindNotifications so a config poll only rewrites their values.
    notifyInputs = new Map();
    notifyFields = new Map();
    notifyDirty = false;
    notifySaving = false;
    constructor() {
        this.tilesEl = document.getElementById("sys-tiles");
        this.infoEl = document.getElementById("sys-info");
        this.infoCardEl = document.getElementById("sys-info-card");
        this.rowsEl = document.getElementById("sys-device-rows");
        this.netCardEl = document.getElementById("sys-network-card");
        this.netActionsEl = document.getElementById("sys-network-actions");
        this.discoveryEl = document.getElementById("sys-discovery-enabled");
        this.authCardEl = document.getElementById("sys-auth-card");
        this.authStateEl = document.getElementById("sys-auth-state");
        this.authTokenEl = document.getElementById("sys-auth-token");
        this.authErrorEl = document.getElementById("sys-auth-error");
        this.authActionsEl = document.getElementById("sys-auth-actions");
        this.notifyCardEl = document.getElementById("sys-notifications-card");
        this.notifyActionsEl = document.getElementById("sys-notify-actions");
        this.notifyEnabledEl = document.getElementById("sys-notify-enabled");
        this.notifyErrorEl = document.getElementById("sys-notify-error");
        this.overridesEl = document.getElementById("sys-overrides");
        this.certCardEl = document.getElementById("sys-cert-card");
        this.certInfoEl = document.getElementById("sys-cert-info");
        this.certFingerprintEl = document.getElementById("sys-cert-fingerprint");
        const btn = document.getElementById("btn-sys-restart");
        if (btn)
            btn.addEventListener("click", () => triggerApplianceRestart());
        this.bindCertificate();
        store.addEventListener("system", (e) => {
            this.system = e.detail;
            this.renderTiles();
            this.renderInfo();
        });
        store.addEventListener("status", (e) => {
            this.status = e.detail;
            this.renderTiles();
            this.renderInfo();
            this.renderOverrides();
            // The certificate is immutable, so fetch it once. This first status event is
            // the load trigger (the token, if any, is settled by now) and each later one
            // retries a transient failure; a 501 sets certUnavailable to stop retrying.
            if (!this.cert && !this.certUnavailable)
                void this.loadCertificate();
        });
        store.addEventListener("devices", (e) => {
            this.renderDeviceRows(e.detail);
        });
        store.addEventListener("config", (e) => {
            const cfg = e.detail;
            if (!this.netDirty && cfg)
                this.populateNetwork(cfg);
            if (!this.authDirty && cfg)
                this.populateAuth(cfg);
            if (!this.notifyDirty && cfg)
                this.populateNotifications(cfg);
        });
        store.addEventListener("loaderror", (e) => {
            const detail = e.detail;
            if (detail.systemFailed)
                this.renderLoadError(detail.message);
            // A config-only failure leaves the network/access/notification cards hidden
            // with no other signal. Surface it so the miss is not invisible; polling
            // recovers the config on a later tick and the cards then appear.
            if (detail.configFailed && !detail.systemFailed) {
                showToast("Could not load the network, access and notification settings. Retrying shortly.", "warn");
            }
        });
        // The open-access banner links to the System view; once it is shown, bring
        // the Access Control card into view and focus its token field so a keyboard
        // user lands on the action rather than at the top of the page.
        document.querySelector("#open-access-banner a")?.addEventListener("click", () => {
            requestAnimationFrame(() => this.focusAuthCard());
        });
        this.bindNetwork();
        this.bindAuth();
        this.bindNotifications();
    }
    // focusAuthCard scrolls the Access Control card into view and moves focus to
    // the token field. Used by the open-access banner link so following it lands on
    // the control that resolves the warning.
    focusAuthCard() {
        if (!this.authCardEl || this.authCardEl.hidden)
            return;
        this.authCardEl.scrollIntoView({ behavior: "smooth", block: "start" });
        // preventScroll: the smooth scroll above already positions the card; a focus
        // scroll would fight it with an instant jump.
        this.authTokenEl?.focus({ preventScroll: true });
    }
    bindCertificate() {
        document.getElementById("btn-cert-copy")?.addEventListener("click", () => {
            const value = this.cert?.fingerprintSha256;
            if (!value || !navigator.clipboard)
                return;
            navigator.clipboard.writeText(value)
                .then(() => showToast("Fingerprint copied."))
                .catch(() => showToast("Copy failed", "error"));
        });
        document.getElementById("btn-cert-download")?.addEventListener("click", () => void this.downloadCertificate());
    }
    // renderOverrides shows or hides the serve-overrides note on the Network card.
    // Each entry explains why a config-view (persisted) value differs from what the
    // appliance is actually running (effective), because a serve CLI flag overrode
    // it for this run. Empty or absent means no override is active.
    renderOverrides() {
        if (!this.overridesEl)
            return;
        const overrides = this.status?.overrides ?? [];
        // Rebuild only when the set actually changes. This runs on every 3s status
        // poll and #sys-overrides is a role=status live region, so an unconditional
        // rebuild would re-announce the unchanged note to a screen reader each tick.
        // Mirrors populateAuth's setText guard on #sys-auth-state.
        const sig = overrides.map((o) => `${o.field}=${o.effective}|${o.persisted}`).join("\n");
        if (sig === this.lastOverridesSig)
            return;
        this.lastOverridesSig = sig;
        this.overridesEl.textContent = "";
        if (overrides.length === 0) {
            this.overridesEl.hidden = true;
            return;
        }
        this.overridesEl.hidden = false;
        this.overridesEl.appendChild(elem("span", "staged-badge", "Serve overrides active"));
        for (const o of overrides) {
            const label = OVERRIDE_LABELS[o.field] ?? o.field;
            const persisted = o.persisted === "" ? "(default)" : o.persisted;
            this.overridesEl.appendChild(elem("span", "override-line", `${label}: serving ${o.effective} (config file: ${persisted})`));
        }
    }
    // loadCertificate fetches the management certificate metadata once. A 501 means
    // the endpoints are not mounted (the appliance could not read its certificate),
    // so it stops retrying; any other failure is left transient for a later retry.
    async loadCertificate() {
        if (this.certPending)
            return;
        this.certPending = true;
        try {
            this.cert = await api.getCertificate();
            this.renderCertificate();
        }
        catch (err) {
            if (err instanceof ApiError && err.status === 501)
                this.certUnavailable = true;
        }
        finally {
            this.certPending = false;
        }
    }
    // renderCertificate fills the certificate panel. The certificate is immutable,
    // so this runs once (unlike the diffed telemetry renders) and rebuilds the grid.
    renderCertificate() {
        const cert = this.cert;
        if (!cert || !this.certCardEl || !this.certInfoEl)
            return;
        this.certCardEl.hidden = false;
        const rows = [
            ["Type", cert.selfSigned ? "Self-signed" : "Custom (CA-signed)"],
            ["Subject", cert.subject],
            ["Issuer", cert.issuer],
            ["Valid from", formatCertTime(cert.notBefore)],
            ["Valid until", formatCertTime(cert.notAfter)],
            ["DNS names", cert.dnsNames.length ? cert.dnsNames.join(", ") : "-"],
            ["IP addresses", cert.ipAddresses.length ? cert.ipAddresses.join(", ") : "-"],
        ];
        this.certInfoEl.textContent = "";
        for (const [k, v] of rows) {
            this.certInfoEl.appendChild(elem("dt", "info-key", k));
            this.certInfoEl.appendChild(elem("dd", "info-val mono", v));
        }
        if (this.certFingerprintEl)
            this.certFingerprintEl.value = cert.fingerprintSha256;
    }
    // downloadCertificate fetches the PEM (bearer-authenticated, so a bare link
    // could not) and saves it via a Blob object URL. The public certificate only;
    // the private key is never fetched.
    async downloadCertificate() {
        const btn = document.getElementById("btn-cert-download");
        // setBusy keeps the button focusable (aria-disabled, not disabled), so guard
        // re-entry against a keyboard re-activation while the fetch is in flight.
        if (btn?.getAttribute("aria-disabled") === "true")
            return;
        if (btn)
            setBusy(btn, "Preparing...");
        try {
            const pem = await api.getCertificatePem();
            const url = URL.createObjectURL(new Blob([pem], { type: "application/x-pem-file" }));
            const a = document.createElement("a");
            a.href = url;
            a.download = "birdnet-go-remote-mic-mgmt.pem";
            document.body.appendChild(a);
            a.click();
            a.remove();
            URL.revokeObjectURL(url);
            showToast("Certificate downloaded.");
        }
        catch (err) {
            showToast(`Download failed: ${apiErrorMessage(err)}`, "error");
        }
        finally {
            if (btn)
                clearBusy(btn, "Download PEM");
        }
    }
    // buildNotifyField builds one threshold input (label, number input with the
    // contract's min/max, error, hint) into its group container and records the
    // input and its .form-field wrapper for later population and error marking. It
    // mirrors the device form's field() helper; the server is the authority on
    // bounds, so there is no live validation here beyond the native min/max.
    buildNotifyField(container, spec) {
        const id = `sys-notify-${spec.key}`;
        const field = elem("div", "form-field");
        const label = elem("label", "field-label", spec.label);
        label.setAttribute("for", id);
        field.appendChild(label);
        const input = document.createElement("input");
        input.type = "number";
        input.className = "field-input mono";
        input.id = id;
        input.min = String(spec.min);
        input.max = String(spec.max);
        input.step = "1";
        // Only hint the digits-only keypad for non-negative fields: a numeric
        // inputMode omits the minus sign on mobile, which would block editing a
        // negative-bounded field (quietDbfs, min -99); its default keyboard keeps it.
        if (spec.min >= 0)
            input.inputMode = "numeric";
        input.setAttribute("aria-describedby", `${id}-err ${id}-hint`);
        input.addEventListener("input", () => this.markNotifyDirty(spec.key));
        field.appendChild(input);
        const error = elem("span", "field-error");
        error.id = `${id}-err`;
        field.appendChild(error);
        const hint = elem("span", "field-hint", spec.hint);
        hint.id = `${id}-hint`;
        field.appendChild(hint);
        container.appendChild(field);
        this.notifyInputs.set(spec.key, input);
        this.notifyFields.set(spec.key, field);
    }
    bindNotifications() {
        const audioEl = document.getElementById("sys-notify-audio-fields");
        const hostEl = document.getElementById("sys-notify-host-fields");
        // Build the inputs once. Without both containers the card cannot render, so
        // leave it hidden rather than half-built.
        if (audioEl && hostEl) {
            for (const spec of NOTIFY_FIELDS) {
                this.buildNotifyField(spec.group === "audio" ? audioEl : hostEl, spec);
            }
        }
        this.notifyEnabledEl?.addEventListener("change", () => {
            this.markNotifyDirty();
            this.applyNotifyEnabledState();
        });
        document.getElementById("btn-notify-save")?.addEventListener("click", () => void this.saveNotifications());
        document.getElementById("btn-notify-discard")?.addEventListener("click", () => void this.discardNotifications());
    }
    // markNotifyDirty stages a change: it reveals the actions bar and clears the
    // edited field's error (and the form-level error) so a stale 422 does not
    // linger over a value the operator has since changed.
    markNotifyDirty(key) {
        this.notifyDirty = true;
        if (this.notifyActionsEl)
            this.notifyActionsEl.hidden = false;
        if (key) {
            this.notifyFields.get(key)?.classList.remove("invalid");
            this.notifyInputs.get(key)?.setAttribute("aria-invalid", "false");
        }
        if (this.notifyErrorEl)
            this.notifyErrorEl.textContent = "";
    }
    // applyNotifyEnabledState greys the thresholds when the monitors are off, so it
    // is clear they are inert. Their values are still read on save (a disabled
    // input keeps its value), so turning the monitors back on preserves them.
    applyNotifyEnabledState() {
        const on = this.notifyEnabledEl?.checked ?? true;
        for (const input of this.notifyInputs.values())
            input.disabled = !on;
    }
    clearNotifyErrors() {
        for (const [key, input] of this.notifyInputs) {
            this.notifyFields.get(key)?.classList.remove("invalid");
            input.setAttribute("aria-invalid", "false");
        }
        if (this.notifyErrorEl)
            this.notifyErrorEl.textContent = "";
    }
    populateNotifications(cfg) {
        if (this.notifyCardEl)
            this.notifyCardEl.hidden = false;
        const n = cfg.notifications;
        const enabled = n?.enabled ?? true;
        if (this.notifyEnabledEl && this.notifyEnabledEl.checked !== enabled) {
            this.notifyEnabledEl.checked = enabled;
        }
        for (const spec of NOTIFY_FIELDS) {
            const input = this.notifyInputs.get(spec.key);
            if (!input)
                continue;
            const group = (spec.group === "audio" ? n?.audio : n?.host);
            const value = group?.[spec.key];
            const text = value === undefined ? "" : String(value);
            // Only write when changed: this runs on every 3 s config poll (while not
            // dirty), and a redundant assignment would disturb a field being read.
            if (input.value !== text)
                input.value = text;
        }
        this.clearNotifyErrors();
        this.notifyDirty = false;
        this.applyNotifyEnabledState();
        if (this.notifyActionsEl)
            this.notifyActionsEl.hidden = true;
    }
    // discardNotifications reverts the thresholds to the saved config, confirming
    // first when there are unsaved edits so a stray click cannot drop them.
    async discardNotifications() {
        if (this.notifyDirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "The notification settings have unsaved changes that will be lost.",
                confirmLabel: "Discard",
                danger: true,
            });
            if (!ok)
                return;
        }
        const cfg = store.getState().config;
        if (cfg)
            this.populateNotifications(cfg);
        // populateNotifications hid the actions bar holding the Discard button focus
        // was on, dropping it to <body>. Return focus to the card's stable top
        // control, matching saveNotifications.
        this.notifyEnabledEl?.focus();
    }
    // saveNotifications persists the whole notifications block. On a 422 it marks
    // the offending input (mapping the server's field path via the core helper) so
    // a clear-above-onset error lands on that input rather than as a bare toast.
    async saveNotifications() {
        if (this.notifySaving)
            return;
        const values = {};
        for (const [key, input] of this.notifyInputs)
            values[key] = input.value;
        // Reject a blank or non-integer box before sending: the server treats an
        // absent field as unchanged, so omitting it (buildNotificationsPatch's
        // defensive behavior) would report "applied" while silently keeping the old
        // value. Mark the offending inputs and abort instead.
        const invalid = unparsedThresholds(values);
        if (invalid.length > 0) {
            this.clearNotifyErrors();
            for (const key of invalid) {
                setFieldError(this.notifyFields.get(key) ?? null, this.notifyInputs.get(key), document.getElementById(`sys-notify-${key}-err`), "Enter a whole number.");
            }
            if (this.notifyErrorEl) {
                this.notifyErrorEl.textContent = "Some thresholds are blank or not whole numbers.";
            }
            this.notifyInputs.get(invalid[0])?.focus();
            return;
        }
        const patch = buildNotificationsPatch(this.notifyEnabledEl?.checked ?? true, values);
        const saveBtn = document.getElementById("btn-notify-save");
        const discardBtn = document.getElementById("btn-notify-discard");
        this.notifySaving = true;
        // Busy affordance that keeps Save focusable (see setBusy); notifySaving guards
        // re-entry.
        if (saveBtn)
            setBusy(saveBtn, "Saving...");
        if (discardBtn)
            discardBtn.disabled = true;
        this.clearNotifyErrors();
        try {
            const res = await api.patchConfig({ notifications: patch });
            this.notifyDirty = false;
            store.applyConfig(res.config);
            await store.refreshStatus();
            if (this.notifyActionsEl)
                this.notifyActionsEl.hidden = true;
            showToast(res.restartRequired
                ? "Notification settings saved. Restart the appliance to finish applying the configuration."
                : "Notification settings applied.", res.restartRequired ? "warn" : "info");
            // Disabling the Save button the operator just activated dropped keyboard
            // focus to <body>, and hiding the actions bar on success removes the
            // landing spot; re-enabling in the finally does not restore focus. Move it
            // to the enabled toggle, the card's stable top control, matching the auth
            // card's focus-restoration convention. The error path leaves focus on the
            // offending input (see showNotifyError), so only the success path does this.
            this.notifyEnabledEl?.focus();
        }
        catch (err) {
            this.showNotifyError(err);
        }
        finally {
            this.notifySaving = false;
            if (saveBtn)
                clearBusy(saveBtn, "Save Changes");
            if (discardBtn)
                discardBtn.disabled = false;
        }
    }
    // showNotifyError maps a failed save onto the form: a validation error marks
    // the named input (or the form-level region when the path is not one the card
    // owns) and refocuses it; any other failure is a toast.
    showNotifyError(err) {
        if (err instanceof ApiError && err.errors && err.errors.length > 0) {
            const item = err.errors[0];
            const spec = item.field ? fieldForServerPath(item.field) : null;
            const reason = item.reason ?? err.title;
            if (spec) {
                this.notifyFields.get(spec.key)?.classList.add("invalid");
                const input = this.notifyInputs.get(spec.key);
                input?.setAttribute("aria-invalid", "true");
                const errEl = document.getElementById(`sys-notify-${spec.key}-err`);
                if (errEl)
                    errEl.textContent = reason;
                input?.focus();
                return;
            }
            if (this.notifyErrorEl) {
                this.notifyErrorEl.textContent = reason;
                return;
            }
        }
        const msg = err instanceof ApiError ? err.title : err instanceof Error ? err.message : String(err);
        showToast(`Save failed: ${msg}`, "error");
    }
    // setAuthReveal shows or hides the token field and keeps the reveal button's
    // label and accessible name in step. It is the single source of the reveal
    // state, used by the reveal toggle, Generate (reveals), and a successful save
    // (re-hides), so the state is never written in two places that could diverge.
    setAuthReveal(show) {
        if (this.authTokenEl)
            this.authTokenEl.type = show ? "text" : "password";
        const reveal = document.getElementById("btn-auth-reveal");
        if (reveal) {
            // The visible label and the accessible name both swap Show/Hide; there is
            // no aria-pressed, so the state is carried by the label rather than by a
            // pressed toggle contradicting a changing label.
            reveal.textContent = show ? "Hide" : "Show";
            reveal.setAttribute("aria-label", show ? "Hide access token" : "Show access token");
        }
    }
    bindAuth() {
        const input = this.authTokenEl;
        if (!input)
            return;
        input.addEventListener("input", () => {
            this.authDirty = true;
            this.setAuthError("");
            if (this.authActionsEl)
                this.authActionsEl.hidden = false;
        });
        // The card is not a <form>, so Enter in the token field would do nothing.
        // Wire it to Save, matching the muscle memory of a single-field form.
        input.addEventListener("keydown", (e) => {
            if (e.key === "Enter") {
                e.preventDefault();
                void this.saveAuth();
            }
        });
        document.getElementById("btn-auth-reveal")?.addEventListener("click", () => {
            this.setAuthReveal(input.type === "password");
        });
        document.getElementById("btn-auth-copy")?.addEventListener("click", () => {
            const value = input.value.trim();
            if (!value) {
                // An empty field the operator just cleared is not (yet) open access; only
                // a saved empty token is. Distinguish the two so the message is accurate.
                showToast(this.authDirty
                    ? "Nothing to copy: the token field is empty. Save to switch to open access."
                    : "No token to copy: the appliance is on open access.", "warn");
                return;
            }
            if (!navigator.clipboard)
                return;
            navigator.clipboard.writeText(value)
                // Flag an unsaved value: Generate hands out a token before it is saved,
                // and copying it into BirdNET-Go before saving here would lock players out.
                .then(() => showToast(this.authDirty
                ? "Access token copied. It is not saved yet: save it here before the players use it."
                : "Access token copied."))
                .catch(() => showToast("Copy failed", "error"));
        });
        document.getElementById("btn-auth-generate")?.addEventListener("click", () => {
            input.value = generateToken();
            // Reveal the generated value: the operator needs to see it to copy it into
            // BirdNET-Go, and a masked random string cannot be verified by eye.
            this.setAuthReveal(true);
            input.dispatchEvent(new Event("input"));
            input.focus();
        });
        document.getElementById("btn-auth-save")?.addEventListener("click", () => void this.saveAuth());
        document.getElementById("btn-auth-discard")?.addEventListener("click", () => void this.discardAuth());
    }
    setAuthError(message) {
        setFieldError(this.authTokenEl?.closest(".form-field") ?? null, this.authTokenEl, this.authErrorEl, message);
    }
    populateAuth(cfg) {
        if (this.authCardEl)
            this.authCardEl.hidden = false;
        const token = cfg.auth?.token ?? "";
        // Only write when changed: this runs on every 3 s config poll (while not
        // dirty) and a redundant assignment to a field the operator has revealed is
        // needless churn.
        if (this.authTokenEl && this.authTokenEl.value !== token)
            this.authTokenEl.value = token;
        if (this.authStateEl) {
            const stateText = token
                ? "Token required: the API, this UI and the RTSP streams ask for credentials."
                : "Open access: anyone on the network can listen and change settings.";
            // #sys-auth-state is a role=status region rewritten on every config event;
            // setText only writes when the text actually changes so a steady state is
            // not re-announced to screen readers each poll.
            setText(this.authStateEl, stateText);
            this.authStateEl.classList.toggle("locked", !!token);
            this.authStateEl.classList.toggle("open", !token);
        }
        this.setAuthError("");
        this.authDirty = false;
        if (this.authActionsEl)
            this.authActionsEl.hidden = true;
    }
    // discardAuth reverts the token field to the saved value, confirming first
    // when there are unsaved edits so a stray click cannot drop a generated token.
    async discardAuth() {
        if (this.authDirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "The access token has unsaved changes that will be lost.",
                confirmLabel: "Discard",
                danger: true,
            });
            if (!ok)
                return;
        }
        const cfg = store.getState().config;
        if (cfg)
            this.populateAuth(cfg);
        // Re-mask the token: Generate reveals it as plaintext, and discarding must not
        // leave the restored saved secret on screen.
        this.setAuthReveal(false);
        // populateAuth hid the actions bar holding the Discard button focus was on,
        // dropping it to <body>; return focus to the token field, matching saveAuth.
        this.authTokenEl?.focus();
    }
    // saveAuth persists the token. Clearing it opens the appliance to the network,
    // so that path confirms first. On success the UI's own stored token is swapped
    // synchronously before any follow-up request, so the next poll already carries
    // the new value and cannot be rejected by the freshly rotated appliance.
    async saveAuth() {
        if (this.authSaving || !this.authTokenEl)
            return;
        const token = this.authTokenEl.value.trim();
        if (!TOKEN_RULE.test(token)) {
            this.setAuthError("Use 12 to 128 characters: letters, digits, and . _ ~ - only.");
            this.authTokenEl.focus();
            return;
        }
        if (!token) {
            const ok = await confirmDialog({
                title: "Allow open access?",
                body: "Removing the token lets anyone on the network listen to the microphones and change settings.",
                confirmLabel: "Allow open access",
                danger: true,
            });
            if (!ok)
                return;
        }
        const saveBtn = document.getElementById("btn-auth-save");
        const discardBtn = document.getElementById("btn-auth-discard");
        this.authSaving = true;
        // Busy affordance that keeps Save focusable (aria-disabled, not disabled), so
        // it does not steal keyboard focus; the authSaving guard blocks re-entry.
        if (saveBtn)
            setBusy(saveBtn, "Saving...");
        if (discardBtn)
            discardBtn.disabled = true;
        // Open the store's rotation window so an in-flight poll rejected while the
        // appliance is switching tokens (it enforces the new one before the PATCH
        // response is fully written) does not pop the login prompt over a working
        // page. It is closed in the finally, after setToken has run.
        store.beginTokenSwap();
        try {
            const res = await api.patchConfig({ auth: { token } });
            this.authDirty = false;
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
            }
            else {
                showToast(token ? "Access token saved. BirdNET-Go and players now need it." : "Open access enabled.", token ? "info" : "warn");
            }
            // Re-hide the token after a successful save: it may have been revealed to
            // copy it, and leaving a saved secret in plain sight is needless exposure.
            this.setAuthReveal(false);
        }
        catch (err) {
            if (err instanceof ApiError && err.errors && err.errors.length > 0) {
                this.setAuthError(err.errors[0].reason ?? err.title);
            }
            else {
                // A non-validation failure (network drop, a lost response) is ambiguous:
                // the appliance applies the token BEFORE it finishes writing the PATCH
                // response, so the new credential may already be in force even though this
                // call looks failed. Warn rather than imply nothing changed.
                showToast(`Could not confirm the token change: ${apiErrorMessage(err)}. The new token may already be in force; if this UI locks you out, reload and sign in with it.`, "warn");
            }
        }
        finally {
            store.endTokenSwap();
            this.authSaving = false;
            if (saveBtn)
                clearBusy(saveBtn, "Save Token");
            if (discardBtn)
                discardBtn.disabled = false;
            // Disabling the Save button the user just activated dropped keyboard focus
            // to <body>; re-enabling does not restore it. After a successful save the
            // actions bar is hidden, so the token input is the sensible landing spot in
            // every case. Restore focus explicitly, matching the convention the toggle
            // and settings paths in dashboard.ts already follow.
            this.authTokenEl?.focus();
        }
    }
    // renderLoadError swaps the telemetry placeholder for the failure cause and a
    // Retry button so the system view is not stuck loading when /system is
    // unreachable. A successful retry re-renders via the system event.
    renderLoadError(message) {
        if (!this.tilesEl)
            return;
        this.tilesEl.textContent = "";
        // The tiles were just detached, so drop their stale refs; otherwise a later
        // renderTiles would reuse detached nodes and the diffed pass would not rebuild.
        this.tileEls.clear();
        const p = elem("p", "cfg-empty");
        this.tilesEl.appendChild(p);
        renderLoadError(p, message, "Loading system telemetry...", () => void store.retry());
    }
    bindNetwork() {
        if (this.discoveryEl) {
            this.discoveryEl.addEventListener("change", () => {
                this.netDirty = true;
                if (this.netActionsEl)
                    this.netActionsEl.hidden = false;
            });
        }
        document.getElementById("btn-network-save")?.addEventListener("click", () => this.saveNetwork());
        document.getElementById("btn-network-discard")?.addEventListener("click", () => void this.discardNetwork());
    }
    // discardNetwork reverts the network form to the saved config, confirming
    // first when there are unsaved edits so a stray click cannot drop them.
    async discardNetwork() {
        if (this.netDirty) {
            const ok = await confirmDialog({
                title: "Discard changes?",
                body: "The network settings have unsaved changes that will be lost.",
                confirmLabel: "Discard",
                danger: true,
            });
            if (!ok)
                return;
        }
        const cfg = store.getState().config;
        if (cfg)
            this.populateNetwork(cfg);
        // populateNetwork hid the actions bar holding the Discard button focus was on,
        // dropping it to <body>; return focus to the discovery toggle.
        this.discoveryEl?.focus();
    }
    populateNetwork(cfg) {
        if (this.netCardEl)
            this.netCardEl.hidden = false;
        // Config is polled every 3 s, so only write an input when its value actually
        // changes: a redundant assignment is wasteful and could disturb a field the
        // operator is reading. These run only while the form is not dirty (guarded by
        // the caller), so they never overwrite an in-progress edit.
        const rtsp = document.getElementById("sys-rtsp-listen");
        const rtspVal = cfg.listen ?? "";
        if (rtsp && rtsp.value !== rtspVal)
            rtsp.value = rtspVal;
        const mgmt = document.getElementById("sys-mgmt-listen");
        const mgmtVal = cfg.management?.listen ?? "(default)";
        if (mgmt && mgmt.value !== mgmtVal)
            mgmt.value = mgmtVal;
        const discovery = cfg.discovery?.enabled ?? true;
        if (this.discoveryEl && this.discoveryEl.checked !== discovery)
            this.discoveryEl.checked = discovery;
        this.netDirty = false;
        if (this.netActionsEl)
            this.netActionsEl.hidden = true;
    }
    async saveNetwork() {
        const saveBtn = document.getElementById("btn-network-save");
        const discardBtn = document.getElementById("btn-network-discard");
        // setBusy keeps Save focusable, so guard re-entry against a keyboard
        // re-activation while the PATCH is in flight, matching the Access Control save.
        if (saveBtn?.getAttribute("aria-disabled") === "true")
            return;
        if (saveBtn)
            setBusy(saveBtn, "Saving...");
        if (discardBtn)
            discardBtn.disabled = true;
        try {
            const res = await api.patchConfig({ discovery: { enabled: this.discoveryEl?.checked ?? true } });
            this.netDirty = false;
            // Seed the cached config with the authoritative PATCH response, matching the
            // auth/notify/device save paths, so a later queued read builds from this
            // change instead of a stale base.
            store.applyConfig(res.config);
            if (this.netActionsEl)
                this.netActionsEl.hidden = true;
            await store.refreshConfig();
            showToast(res.restartRequired ? "Discovery setting saved. Restart the appliance to apply." : "Discovery setting applied.");
            // Hiding the actions bar dropped focus from the Save button; return it to
            // the discovery toggle, the card's editable control.
            this.discoveryEl?.focus();
        }
        catch (err) {
            showToast(`Save failed: ${apiErrorMessage(err)}`, "error");
        }
        finally {
            if (saveBtn)
                clearBusy(saveBtn, "Save Changes");
            if (discardBtn)
                discardBtn.disabled = false;
        }
    }
    // buildTile creates one resource-gauge tile with stable inner nodes (the sub,
    // value, unit and progress bar are always present and toggled/updated, never
    // rebuilt), so renderTiles can update it in place across polls. The label is
    // fixed per tile key, so it is written once here.
    buildTile(label) {
        const tile = elem("div", "system-tile");
        const header = elem("div", "tile-header");
        const sub = elem("span", "mono");
        header.append(elem("span", undefined, label), sub);
        tile.appendChild(header);
        const val = elem("div", "tile-value mono");
        const value = elem("span");
        const unit = elem("span", "telemetry-unit");
        val.append(value, unit);
        tile.appendChild(val);
        const bar = elem("div", "progress-bar-bg");
        const barFill = elem("div", "progress-bar-fill");
        bar.appendChild(barFill);
        tile.appendChild(bar);
        return { tile, sub, value, unit, bar, barFill };
    }
    updateTile(refs, spec) {
        setText(refs.sub, spec.sub);
        setHidden(refs.sub, !spec.sub);
        setText(refs.value, spec.value);
        setText(refs.unit, spec.unit);
        setHidden(refs.unit, !spec.unit);
        if (spec.barPct === undefined) {
            setHidden(refs.bar, true);
        }
        else {
            setHidden(refs.bar, false);
            const w = `${Math.min(100, Math.max(0, spec.barPct)).toFixed(1)}%`;
            if (refs.barFill.style.width !== w)
                refs.barFill.style.width = w;
        }
    }
    // The tile grid holds only the live resource gauges. Host and appliance facts
    // live in the separate System Information card below. Rendered with the diffed
    // convention: tiles are keyed by a stable key, updated in place, and only
    // added/removed/reordered when the present set changes.
    renderTiles() {
        if (!this.tilesEl)
            return;
        const grid = this.tilesEl;
        const sys = this.system;
        if (!sys)
            return;
        // Remove a load-error placeholder renderLoadError may have left in the grid, so
        // a recovered poll does not strand it among the gauges: the diffed pass below
        // tracks only tile nodes, not this foreign child.
        grid.querySelector(":scope > .cfg-empty")?.remove();
        const specs = [];
        specs.push({
            key: "cpu", label: "CPU Utilization", sub: sys.cpuCores > 0 ? `${sys.cpuCores} Cores` : "",
            value: sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a", unit: "%", barPct: sys.cpuPercent,
        });
        if (sys.memTotalBytes > 0) {
            specs.push({
                key: "mem", label: "Memory", sub: `${Math.round(sys.memTotalBytes / 1048576)} MB Total`,
                value: String(Math.round(sys.memUsedBytes / 1048576)), unit: "MB used",
                barPct: (sys.memUsedBytes / sys.memTotalBytes) * 100,
            });
        }
        specs.push({
            key: "temp", label: "SoC Temperature", sub: "",
            value: sys.tempCelsius !== undefined ? sys.tempCelsius.toFixed(1) : "n/a", unit: "deg C",
            barPct: sys.tempCelsius !== undefined ? (sys.tempCelsius / 85) * 100 : undefined,
        });
        if (sys.diskTotalBytes > 0) {
            specs.push({
                key: "disk", label: "Disk", sub: `${(sys.diskTotalBytes / 1073741824).toFixed(1)} GB Total`,
                value: (sys.diskUsedBytes / 1073741824).toFixed(1), unit: "GB used",
                barPct: (sys.diskUsedBytes / sys.diskTotalBytes) * 100,
            });
        }
        const want = new Set(specs.map((s) => s.key));
        for (const [key, refs] of this.tileEls) {
            if (!want.has(key)) {
                refs.tile.remove();
                this.tileEls.delete(key);
            }
        }
        let prev = null;
        for (const spec of specs) {
            let refs = this.tileEls.get(spec.key);
            if (!refs) {
                refs = this.buildTile(spec.label);
                this.tileEls.set(spec.key, refs);
            }
            this.updateTile(refs, spec);
            const target = prev ? prev.nextSibling : grid.firstChild;
            if (refs.tile !== target)
                grid.insertBefore(refs.tile, target);
            prev = refs.tile;
        }
    }
    // renderInfo fills the System Information label/value grid, diffed: rows are
    // keyed by label, values updated in place, and dt/dd pairs added, removed and
    // ordered only on change rather than clearing the grid every poll.
    renderInfo() {
        if (!this.infoEl)
            return;
        const grid = this.infoEl;
        const sys = this.system;
        const st = this.status;
        if (!sys && !st)
            return;
        const rows = [];
        if (sys) {
            rows.push(["Hostname", sys.hostname || "-"]);
            rows.push(["Platform", sys.platform || "-"]);
            if (sys.cpuModel)
                rows.push(["CPU", `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`]);
            if (sys.os)
                rows.push(["OS", sys.os]);
            if (sys.kernel)
                rows.push(["Kernel", sys.kernel]);
        }
        if (st) {
            rows.push(["Version", st.version || "-"]);
            rows.push(["Uptime", formatUptime(st.uptimeSeconds)]);
            rows.push(["RTSP Listen", st.rtspListen]);
            rows.push(["Devices Serving", `${st.devicesServing} / ${st.devicesTotal}`]);
        }
        const want = new Set(rows.map(([k]) => k));
        for (const [key, pair] of this.infoRows) {
            if (!want.has(key)) {
                pair.dt.remove();
                pair.dd.remove();
                this.infoRows.delete(key);
            }
        }
        let prev = null; // previous row's dd
        for (const [k, v] of rows) {
            let pair = this.infoRows.get(k);
            if (!pair) {
                pair = { dt: elem("dt", "info-key", k), dd: elem("dd", "info-val mono", v) };
                this.infoRows.set(k, pair);
            }
            else {
                setText(pair.dd, v);
            }
            const dtTarget = prev ? prev.nextSibling : grid.firstChild;
            if (pair.dt !== dtTarget)
                grid.insertBefore(pair.dt, dtTarget);
            if (pair.dd !== pair.dt.nextSibling)
                grid.insertBefore(pair.dd, pair.dt.nextSibling);
            prev = pair.dd;
        }
        if (this.infoCardEl)
            this.infoCardEl.hidden = rows.length === 0;
    }
    buildDeviceRow() {
        const tr = document.createElement("tr");
        const name = this.td("");
        const alsa = this.td("", true);
        const path = this.td("", true);
        const codec = this.td("", true);
        const client = this.td("", true);
        const stateTd = document.createElement("td");
        const stateSpan = elem("span");
        stateTd.appendChild(stateSpan);
        tr.append(name, alsa, path, codec, client, stateTd);
        return { tr, name, alsa, path, codec, client, stateSpan };
    }
    updateDeviceRow(r, d) {
        setText(r.name, d.name);
        setText(r.alsa, d.device);
        setText(r.path, d.path);
        const rate = d.negotiatedRate ?? d.rate;
        setText(r.codec, `${modeLabel(d.mode)} ${rate.toLocaleString("en-US")} Hz`);
        setText(r.client, d.clientConnected ? "Connected" : "-");
        const badge = deviceStateBadge(d.state);
        if (r.stateSpan.className !== badge.cls)
            r.stateSpan.className = badge.cls;
        setText(r.stateSpan, badge.label);
    }
    // renderDeviceRows fills the Stream-Status table, diffed: rows are keyed by the
    // immutable ALSA device id, cells updated in place, and rows added, removed and
    // ordered only on change rather than rebuilding the whole tbody every poll.
    renderDeviceRows(devices) {
        if (!this.rowsEl)
            return;
        const body = this.rowsEl;
        if (devices.length === 0) {
            for (const r of this.deviceRows.values())
                r.tr.remove();
            this.deviceRows.clear();
            if (!this.deviceEmptyRow) {
                const tr = document.createElement("tr");
                const td = elem("td", undefined, "No devices configured.");
                td.setAttribute("colspan", "6");
                tr.appendChild(td);
                this.deviceEmptyRow = tr;
            }
            if (this.deviceEmptyRow.parentNode !== body)
                body.appendChild(this.deviceEmptyRow);
            return;
        }
        // Non-empty: drop the placeholder row if it is showing.
        if (this.deviceEmptyRow?.parentNode)
            this.deviceEmptyRow.remove();
        const want = new Set(devices.map((d) => d.device));
        for (const [id, r] of this.deviceRows) {
            if (!want.has(id)) {
                r.tr.remove();
                this.deviceRows.delete(id);
            }
        }
        let prev = null;
        for (const d of devices) {
            let r = this.deviceRows.get(d.device);
            if (!r) {
                r = this.buildDeviceRow();
                this.deviceRows.set(d.device, r);
            }
            this.updateDeviceRow(r, d);
            const target = prev ? prev.nextSibling : body.firstChild;
            if (r.tr !== target)
                body.insertBefore(r.tr, target);
            prev = r.tr;
        }
    }
    td(text, mono = false) {
        return elem("td", mono ? "mono" : undefined, text);
    }
}
