import { api, ApiError, apiErrorMessage, failureReason, isRefusal, problemFor, problemReason } from "../../lib/api.ts";
import { clearBusy, copyText, downloadBlob, infoRow, part, renderLoadError, setBusy, setFieldError, setLoading, setText, showUnconfirmed } from "../../lib/ui.ts";
import { confirmDialog } from "../../lib/modal.ts";
import { CERT_LABELS, certRows, certTooLargeReason, parseExtraSans, type CertLabel } from "../../lib/certificate-core.ts";
import { sentence } from "../../lib/text.ts";
import { showToast } from "../../components/toast.ts";
import type { CertificateInfo } from "../../lib/types.ts";

// CERT_LOAD_ERROR_THRESHOLD is the number of consecutive certificate load
// failures (one attempt per status poll) before the card shows its load-error
// message with a Retry button. A single blip self-heals on the next poll
// without any operator-visible noise.
const CERT_LOAD_ERROR_THRESHOLD = 3;

// CertificateCard is the Management Certificate card (#sys-cert-card): the
// certificate metadata and fingerprint, the PEM download, and the regenerate
// and install flows.
export class CertificateCard {
  private readonly cardEl: HTMLElement | null;
  private readonly infoEl: HTMLElement | null;
  private readonly fingerprintEl: HTMLInputElement | null;
  private readonly errorEl: HTMLElement | null;
  private readonly sansEl: HTMLInputElement | null;
  private readonly sansErrorEl: HTMLElement | null;
  private readonly pemEl: HTMLTextAreaElement | null;
  private readonly pemErrorEl: HTMLElement | null;
  private readonly keyEl: HTMLTextAreaElement | null;
  private readonly keyErrorEl: HTMLElement | null;
  // The value cell of each metadata row, laid out once for the fixed set of
  // rows and filled in place.
  private readonly values = new Map<CertLabel, HTMLElement>();
  // cert is the management certificate metadata. It changes at runtime: the
  // card fetches it on every status poll (a regenerate, an install, or an
  // external change swaps the certificate live, no restart) and re-fetches
  // after a regenerate or install. pending guards concurrent loads;
  // unavailable is set on a 501 so a permanently-unmounted endpoint is not
  // polled forever; busy guards the regenerate and install actions against
  // re-entry (they share it: both replace the certificate). gen is bumped
  // by each successful mutation so a GET that was already in flight when the
  // mutation completed is discarded instead of overwriting the fresh metadata.
  private cert: CertificateInfo | null = null;
  private pending = false;
  private unavailable = false;
  private busy = false;
  private gen = 0;
  // failures counts consecutive load failures; at CERT_LOAD_ERROR_THRESHOLD
  // the card shows the load-error region (role=alert) once. errorShown keeps
  // later silent retries from re-rendering, and so re-announcing, that region.
  private failures = 0;
  private errorShown = false;

  constructor(root: HTMLElement | null) {
    this.cardEl = root;
    this.infoEl = part(root, "sys-cert-info");
    this.fingerprintEl = part<HTMLInputElement>(root, "sys-cert-fingerprint");
    this.errorEl = part(root, "sys-cert-error");
    this.sansEl = part<HTMLInputElement>(root, "sys-cert-extra-sans");
    this.sansErrorEl = part(root, "sys-cert-extra-sans-error");
    this.pemEl = part<HTMLTextAreaElement>(root, "sys-cert-pem");
    this.pemErrorEl = part(root, "sys-cert-pem-error");
    this.keyEl = part<HTMLTextAreaElement>(root, "sys-cert-key");
    this.keyErrorEl = part(root, "sys-cert-key-error");
    for (const label of CERT_LABELS) {
      const { dt, dd } = infoRow(label, "", { mono: true });
      this.infoEl?.append(dt, dd);
      this.values.set(label, dd);
    }
    // Laid out from the first paint, and usable once the first load fills it
    // or fails for good.
    if (root) setLoading(root, true);
    this.bind();
  }

  // load reloads the certificate unless the endpoints are known to be
  // unmounted (a 501).
  public load(): void {
    if (!this.unavailable) void this.fetch();
  }

  private bind(): void {
    part(this.cardEl, "btn-cert-copy")?.addEventListener("click", () => {
      const value = this.cert?.fingerprintSha256;
      if (!value) {
        // The card is in its load-error state (this.cert is null), so a silent
        // no-op would read as a broken button; say why, matching the auth card.
        showToast("No fingerprint to copy: the certificate details could not be loaded.", "warn");
        return;
      }
      copyText(value, "Fingerprint copied.");
    });
    part(this.cardEl, "btn-cert-download")?.addEventListener("click", () => void this.download());
    part(this.cardEl, "btn-cert-regenerate")?.addEventListener("click", () => void this.regenerate());
    part(this.cardEl, "btn-cert-install")?.addEventListener("click", () => void this.install());
    // Clear a field's error as soon as it is edited so a stale rejection does
    // not linger over a value the operator has since changed (mirrors AccessCard).
    this.sansEl?.addEventListener("input", () => this.setFieldError(this.sansEl, this.sansErrorEl, ""));
    this.pemEl?.addEventListener("input", () => this.setFieldError(this.pemEl, this.pemErrorEl, ""));
    this.keyEl?.addEventListener("input", () => this.setFieldError(this.keyEl, this.keyErrorEl, ""));
  }

  // setFieldError marks or clears one field through the shared setFieldError,
  // resolving the .form-field wrapper from the control the same way
  // AccessCard's setError does.
  private setFieldError(input: HTMLElement | null, errorEl: HTMLElement | null, message: string): void {
    setFieldError(input?.closest(".form-field") ?? null, input, errorEl, message);
  }

  // clearLoadError resets the load-failure state and swaps the load-error
  // region back for the info grid. Fresh metadata from any source (a poll, the
  // Retry button, a regenerate or an install) routes through here so the card
  // never keeps showing a stale error over data it now has.
  private clearLoadError(): void {
    this.failures = 0;
    this.errorShown = false;
    if (this.errorEl) {
      this.errorEl.hidden = true;
      this.errorEl.removeAttribute("role");
      this.errorEl.textContent = "";
    }
    if (this.infoEl) this.infoEl.hidden = false;
  }

  // fetch loads the management certificate metadata. It is re-callable: every
  // status poll while the System view shows, arriving on that view, the Retry
  // button, and a regenerate or install (to reconcile the card with what the
  // appliance now serves) all route through here. A 501 means the endpoints are
  // not mounted (the appliance could not read its certificate), so it stops
  // retrying. Any other failure counts toward CERT_LOAD_ERROR_THRESHOLD, at
  // which the card surfaces its load-error region exactly once; the polls keep
  // retrying silently after that (the card self-heals) without re-rendering,
  // and so re-announcing, the alert. A response that resolves after a mutation
  // bumped gen is stale (it describes the certificate that was just replaced)
  // and is dropped.
  private async fetch(): Promise<void> {
    if (this.pending) return;
    this.pending = true;
    const gen = this.gen;
    try {
      const info = await api.getCertificate();
      if (this.gen !== gen) return;
      this.cert = info;
      this.clearLoadError();
      this.render();
    } catch (err: unknown) {
      if (this.gen !== gen) return;
      // Only the appliance's own 501 means it has no certificate control; a
      // proxy's says nothing about it.
      if (isRefusal(err) && err.status === 501) {
        this.unavailable = true;
        // Not busy any more: it turned out not to apply.
        if (this.cardEl) {
          setLoading(this.cardEl, false);
          this.cardEl.hidden = true;
        }
        return;
      }
      this.failures++;
      if (this.failures >= CERT_LOAD_ERROR_THRESHOLD && !this.errorShown && this.errorEl) {
        this.errorShown = true;
        if (this.cardEl) setLoading(this.cardEl, false);
        if (this.infoEl) this.infoEl.hidden = true;
        renderLoadError(this.errorEl, "Certificate details could not be loaded.", "Loading certificate...", () => {
          // The Retry button swapped the alert for its loading text; let a
          // failed manual retry re-render (and re-announce) the error instead
          // of leaving the loading text up with no button.
          this.errorShown = false;
          void this.fetch();
        });
      }
    } finally {
      this.pending = false;
    }
  }

  // render fills the card's values in place. It runs on every successful load
  // (the certificate changes on a regenerate or install).
  private render(): void {
    const cert = this.cert;
    if (!cert || !this.cardEl) return;
    setLoading(this.cardEl, false);
    for (const [label, value] of certRows(cert)) {
      const dd = this.values.get(label);
      if (dd) setText(dd, value);
    }
    if (this.fingerprintEl && this.fingerprintEl.value !== cert.fingerprintSha256) this.fingerprintEl.value = cert.fingerprintSha256;
  }

  // download fetches the PEM (bearer-authenticated, so a bare link could not)
  // and saves it via a Blob object URL. The public certificate only; the
  // private key is never fetched.
  private async download(): Promise<void> {
    const btn = part<HTMLButtonElement>(this.cardEl, "btn-cert-download");
    // setBusy keeps the button focusable (aria-disabled, not disabled), so guard
    // re-entry against a keyboard re-activation while the fetch is in flight.
    if (btn?.getAttribute("aria-disabled") === "true") return;
    if (btn) setBusy(btn, "Preparing...");
    try {
      const pem = await api.getCertificatePem();
      downloadBlob(new Blob([pem], { type: "application/x-pem-file" }), "birdnet-go-remote-mic-mgmt.pem");
      showToast("Certificate downloaded.");
    } catch (err: unknown) {
      showToast(`Download failed: ${failureReason(err)}`, "error");
    } finally {
      if (btn) clearBusy(btn, "Download PEM");
    }
  }

  // regenerate asks the appliance to mint a new self-signed certificate, with
  // any extra SANs from the input, and swap it in live. The card is updated
  // from the response and then reconciled with a fresh GET.
  private async regenerate(): Promise<void> {
    const btn = part<HTMLButtonElement>(this.cardEl, "btn-cert-regenerate");
    // setBusy keeps the button focusable (aria-disabled, not disabled), so guard
    // re-entry against a keyboard re-activation while a request is in flight.
    if (this.busy || btn?.getAttribute("aria-disabled") === "true") return;
    const parsed = parseExtraSans(this.sansEl?.value ?? "");
    if (parsed.error) {
      this.setFieldError(this.sansEl, this.sansErrorEl, parsed.error);
      this.sansEl?.focus();
      return;
    }
    // Unknown (the metadata never loaded) is treated as the common
    // appliance-managed case so the wording and the danger styling agree.
    const managed = this.cert?.managed ?? true;
    const ok = await confirmDialog({
      title: "Regenerate certificate?",
      body: `${managed
        ? "A new self-signed certificate replaces the current one."
        : "This replaces the operator-installed certificate with a self-signed one."} New connections use it immediately; this page may show a certificate warning on its next load, and clients that trusted the old fingerprint must trust the new one.`,
      confirmLabel: "Regenerate",
      danger: !managed,
    });
    if (!ok) return;
    this.busy = true;
    if (btn) setBusy(btn, "Regenerating...");
    try {
      // The contract requires a JSON body, so no extras still sends {}.
      const info = await api.regenerateCertificate(parsed.sans.length ? { extraSans: parsed.sans } : {});
      this.cert = info;
      this.gen++;
      this.clearLoadError();
      this.render();
      void this.fetch();
      showToast("Certificate regenerated and applied to new connections. Download and trust the new certificate where needed.");
    } catch (err: unknown) {
      if (!isRefusal(err)) {
        // Anything but a refusal (a dropped connection, or an answer that
        // could not be read) says nothing about whether the appliance applied
        // the change; reconcile from the server instead of reporting a failure
        // that may not be one.
        showUnconfirmed("the certificate change", "refreshing the current certificate");
        void this.fetch();
        return;
      }
      const item = problemFor(err, (e) => e.field?.startsWith("extraSans") ?? false);
      if (item) {
        this.setFieldError(this.sansEl, this.sansErrorEl, sentence(item.reason));
        this.sansEl?.focus();
      } else {
        showToast(`Regenerate failed: ${apiErrorMessage(err)}`, "error");
      }
    } finally {
      this.busy = false;
      if (btn) clearBusy(btn, "Regenerate");
    }
  }

  // install uploads an operator-supplied certificate and private key. The key
  // travels only in the request body and is never echoed into a toast, an error
  // message, or the console. The key textarea is cleared on every completion
  // (success or failure) so the secret does not linger in the DOM; the
  // certificate textarea is public and is cleared only on success, so a
  // rejected certificate stays in place for the operator to correct.
  private async install(): Promise<void> {
    const btn = part<HTMLButtonElement>(this.cardEl, "btn-cert-install");
    if (this.busy || btn?.getAttribute("aria-disabled") === "true") return;
    const certPem = this.pemEl?.value.trim() ?? "";
    const keyPem = this.keyEl?.value.trim() ?? "";
    if (!certPem || !keyPem) {
      if (!certPem) this.setFieldError(this.pemEl, this.pemErrorEl, "Paste the PEM.");
      if (!keyPem) this.setFieldError(this.keyEl, this.keyErrorEl, "Paste the PEM.");
      (certPem ? this.keyEl : this.pemEl)?.focus();
      return;
    }
    const ok = await confirmDialog({
      title: "Install certificate?",
      body: "The appliance will stop managing its certificate: it will not regenerate one automatically on an address change or expiry. New connections use the installed certificate immediately; your browser may warn until it is trusted.",
      confirmLabel: "Install",
      danger: true,
    });
    if (!ok) return;
    this.busy = true;
    if (btn) setBusy(btn, "Installing...");
    try {
      const info = await api.installCertificate({ certPem, keyPem });
      if (this.pemEl) this.pemEl.value = "";
      this.setFieldError(this.pemEl, this.pemErrorEl, "");
      this.setFieldError(this.keyEl, this.keyErrorEl, "");
      this.cert = info;
      this.gen++;
      this.clearLoadError();
      this.render();
      void this.fetch();
      showToast("Custom certificate installed and applied to new connections.");
    } catch (err: unknown) {
      // The API caps request bodies, and a full CA bundle pasted with the
      // certificate is the usual way past the cap, so say what to trim rather
      // than echoing the bare "payload too large". The limit itself comes from
      // the problem detail, so this text cannot drift from the server's value.
      // A 413 is a refusal from whoever sent it: a proxy's means the request
      // never reached the appliance.
      if (err instanceof ApiError && err.status === 413) {
        showToast(`Install failed: ${certTooLargeReason(err.problemDetail)}. Paste only the server certificate and its intermediates, not a full CA bundle, then paste the key again.`, "error");
        return;
      }
      if (!isRefusal(err)) {
        // Same as regenerate: anything but a refusal leaves the outcome
        // unknown, so reconcile rather than claim a failure. The key
        // textarea is still cleared in finally.
        showUnconfirmed("the certificate change", "refreshing the current certificate");
        void this.fetch();
        return;
      }
      let pemBad = false;
      let keyBad = false;
      if (err.errors) {
        for (const item of err.errors) {
          const reason = sentence(problemReason(err, item));
          if (item.field === "certPem") {
            this.setFieldError(this.pemEl, this.pemErrorEl, reason);
            pemBad = true;
          } else if (item.field === "keyPem") {
            this.setFieldError(this.keyEl, this.keyErrorEl, reason);
            keyBad = true;
          }
        }
      }
      if (pemBad || keyBad) {
        // Land on the first invalid field in form order.
        (pemBad ? this.pemEl : this.keyEl)?.focus();
      } else {
        showToast(`Install failed: ${apiErrorMessage(err)}`, "error");
      }
    } finally {
      // Drop the private key from the DOM whether or not the install succeeded:
      // a rejected key must not sit in a hidden textarea until the next attempt.
      // Only the value is cleared; a keyPem field error set in the catch above
      // is left in place so the operator still sees why it was rejected.
      if (this.keyEl) this.keyEl.value = "";
      this.busy = false;
      if (btn) clearBusy(btn, "Install");
    }
  }
}
