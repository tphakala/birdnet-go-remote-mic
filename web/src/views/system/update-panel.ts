import { api, apiErrorMessage, failureReason, isRefusal } from "../../lib/api.ts";
import { store } from "../../lib/store.ts";
import { clearBusy, externalLink, part, setBusy, setHidden, setText, showUnconfirmed } from "../../lib/ui.ts";
import { confirmDialog } from "../../lib/modal.ts";
import { showUpdateModal, type UpdateModal } from "../../components/restart-modal.ts";
import { describeUpdate, followEndText, safeNotesUrl, TickGuard, UpdateFollow, updateUnderway, VersionWatch, withChecksSetting } from "../../lib/update-core.ts";
import { showToast } from "../../components/toast.ts";
import type { UpdateStatus } from "../../lib/types.ts";

// VERSION_NOTICE_MS keeps the "reload onto the new version" notice up long
// enough to be seen by someone who comes back to the tab.
const VERSION_NOTICE_MS = 5 * 60_000;

// UpdatePanel is the software update part of the System Information card: the
// status lines, the daily check switch, Check Now and Update, following an
// update this tab started, and noticing the appliance running a new version.
export class UpdatePanel {
  private readonly root: HTMLElement | null;
  private readonly checkEl: HTMLInputElement | null;
  private readonly checkBtn: HTMLElement | null;
  private readonly applyBtn: HTMLElement | null;
  // The latest update status from a system read; undefined when the appliance
  // sends none.
  private update: UpdateStatus | undefined;
  // Set by the first system read or a failed first load. Until then the
  // status and footer keep their place, empty, so the read fills them in
  // place.
  private settled = false;
  // The release notes address the footer's link was built for.
  private notesUrl = "";
  // Requests in flight from this card; a render never undoes their busy state.
  private checking = false;
  private applying = false;
  private toggling = false;
  // The update this tab started, followed from the request until it ends or
  // the page reloads onto the new version; null when not following.
  private follow: UpdateFollow | null = null;
  private modal: UpdateModal | null = null;
  private followTimer: ReturnType<typeof setInterval> | null = null;
  // The version this page's scripts came from, and the last new version this
  // tab was told about, so the reload notice shows once per version.
  private readonly versionWatch = new VersionWatch();
  private versionNoticeFor = "";
  private reloading = false;

  constructor(root: HTMLElement | null) {
    this.root = root;
    this.checkEl = part<HTMLInputElement>(root, "sys-update-check");
    this.checkBtn = part(root, "btn-update-check");
    this.applyBtn = part(root, "btn-update-apply");
    this.checkEl?.addEventListener("change", () => void this.saveCheck());
    this.checkBtn?.addEventListener("click", () => void this.checkNow());
    this.applyBtn?.addEventListener("click", () => void this.start());
  }

  // system takes the update status from a system read and renders it.
  public system(update: UpdateStatus | undefined): void {
    this.update = update;
    this.settled = true;
    this.render();
  }

  // settle ends the wait after a failed first load: with no status the panel
  // hides, as for an appliance that sends none.
  public settle(): void {
    if (this.settled) return;
    this.settled = true;
    this.render();
  }

  // version notices the appliance running another version than this page
  // loaded against. A tab following its own update shows the wait as soon as
  // the new version answers and reloads once it has settled; any other tab (or
  // one whose follow already ended) is told to reload once it has settled,
  // never reloaded under unsaved input.
  public version(version: string, uptimeSeconds: number): void {
    // Only a version that has settled counts: one the updater rolls back
    // reloads nothing and announces nothing.
    const change = this.versionWatch.seen(version, uptimeSeconds);
    if (change === "same") return;
    if (this.follow) {
      // The new version answers but has not settled: show the wait, so the
      // page is not used in the seconds before it reloads.
      if (change === "changed") {
        // The install was reached even if its short phase was never read.
        this.follow.installReached();
        this.showInstallModal(version);
      } else {
        this.reloadOntoNewVersion(version);
      }
      return;
    }
    if (change !== "confirmed") return;
    if (version === this.versionNoticeFor) return;
    this.versionNoticeFor = version;
    showToast(`The appliance now runs ${version}. Reload this page to use its web UI.`, "warn", VERSION_NOTICE_MS);
  }

  // render patches the update status and footer of the System Information card
  // from the store's status (a check or an update request merges its response
  // there first); InfoCard adds the release rows. An appliance without update
  // support sends none and both stay hidden.
  private render(): void {
    if (!this.settled) return;
    const u = this.update;
    const state = part(this.root, "sys-update-state");
    if (state) setHidden(state, !u);
    const actions = part(this.root, "sys-update-actions");
    if (actions) setHidden(actions, !u?.supported);
    if (!u) return;
    const view = describeUpdate(u);

    const headline = part(this.root, "sys-update-headline");
    if (headline) {
      const cls = `update-headline tone-${view.tone}`;
      if (headline.className !== cls) headline.className = cls;
      setText(headline, view.headline);
      setHidden(headline, view.headline === "");
    }
    const detail = part(this.root, "sys-update-detail");
    if (detail) {
      setText(detail, view.detail);
      setHidden(detail, view.detail === "");
    }
    // The status region stays in place for announcements; with nothing to
    // say it takes no room.
    state?.classList.toggle("is-empty", view.headline === "");
    for (const [id, text] of [["sys-update-hint", view.hint], ["sys-update-note", view.note]] as const) {
      const el = part(this.root, id);
      if (!el) continue;
      setText(el, text);
      setHidden(el, text === "");
    }

    // A build that names no release never checks, so the footer (hidden above)
    // with its switch and actions would only contradict the headline.
    if (this.checkEl && !this.toggling) this.checkEl.checked = u.checkEnabled;
    // Turning checks off stops a download (not an install already handed to
    // the root updater), so say so while one runs.
    const checkHint = part(this.root, "sys-update-check-hint");
    if (checkHint) {
      const warn = u.phase === "downloading" ? "Turning this off stops the download under way." : "";
      setText(checkHint, warn);
      setHidden(checkHint, warn === "");
    }
    this.renderNotes(u.available ? safeNotesUrl(u.notesUrl) : "");

    // Check Now stays in place (hiding a focused button would drop focus) and
    // shows as unavailable while it cannot run.
    const check = this.checkBtn;
    if (check) {
      if (!this.checking) {
        if (view.canCheck) check.removeAttribute("aria-disabled");
        else check.setAttribute("aria-disabled", "true");
      }
    }
    const apply = this.applyBtn;
    if (apply) {
      const hide = view.applyVersion === "" && !view.busy;
      // Update can vanish under focus (checks turned off in another tab);
      // Check Now beside it keeps the keyboard in the card.
      if (hide && !apply.hidden && document.activeElement === apply) this.checkBtn?.focus({ preventScroll: true });
      setHidden(apply, hide);
      if (view.busy) setBusy(apply, u.phase === "installing" ? "Installing..." : u.phase === "downloading" ? "Downloading..." : "Updating...");
      else if (!this.applying && view.applyVersion) clearBusy(apply, `Update to ${view.applyVersion}`);
    }
    this.followStatus(u);
  }

  // renderNotes shows the release notes link for the offered release, rebuilt
  // only when the address changes.
  private renderNotes(url: string): void {
    const slot = part(this.root, "sys-update-notes");
    if (!slot || url === this.notesUrl) return;
    this.notesUrl = url;
    slot.replaceChildren(...(url ? [externalLink(url, "Release Notes")] : []));
  }

  // followStatus feeds each status to the update this tab follows. The store
  // drops reads started before the request, so every status here is current.
  private followStatus(u: UpdateStatus): void {
    const step = this.follow?.status(u);
    if (step === "show") this.showInstallModal(u.latestVersion ?? "the update");
    else if (step === "end") this.endFollow(u);
  }

  private startFollow(fromVersion: string): void {
    this.follow = new UpdateFollow(fromVersion);
    this.versionWatch.rebase(fromVersion);
  }

  // showInstallModal shows the install modal and runs its timer and deadline.
  // A restart holding the modal leaves the follow without one; the version
  // watch still reloads the page.
  private showInstallModal(target: string): void {
    if (this.modal) return;
    this.modal = showUpdateModal(target);
    if (!this.modal) return;
    const shownAt = Date.now();
    this.follow?.shown(shownAt);
    // A deadline read after the page was hidden or asleep says nothing about
    // the install until a fresh status has had a chance to arrive.
    const guard = new TickGuard(shownAt);
    this.followTimer = setInterval(() => {
      const now = Date.now();
      if (!document.hidden) this.modal?.elapsed(now - shownAt);
      if (!guard.mayCheck(now, document.hidden)) return;
      if (this.follow?.tick(now) === "timeout") {
        this.stopFollowTimer();
        this.modal?.overdue();
      }
    }, 1000);
  }

  private stopFollowTimer(): void {
    if (this.followTimer !== null) clearInterval(this.followTimer);
    this.followTimer = null;
  }

  // endFollow ends a follow whose attempt stopped without a restart: the modal
  // (if it showed) closes, focus goes back to the card's retry control, and a
  // notice says why.
  private endFollow(u: UpdateStatus): void {
    const hadModal = this.modal !== null;
    const reachedInstall = this.follow?.reachedInstall ?? false;
    this.stopFollowTimer();
    this.modal?.hide();
    this.modal = null;
    this.follow = null;
    const { text, tone } = followEndText(u, reachedInstall);
    showToast(text, tone);
    if (hadModal) {
      const apply = this.applyBtn;
      (apply && !apply.hidden ? apply : this.checkBtn)?.focus({ preventScroll: true });
    }
  }

  private reloadOntoNewVersion(version: string): void {
    if (this.reloading) return;
    this.reloading = true;
    this.stopFollowTimer();
    this.modal ??= showUpdateModal(version);
    this.modal?.reloading();
    setTimeout(() => window.location.reload(), 600);
  }

  private async saveCheck(): Promise<void> {
    const input = this.checkEl;
    if (!input) return;
    // A flip while the last one is still saving is undone at once rather than
    // left showing a state that was never sent.
    if (this.toggling) {
      input.checked = !input.checked;
      return;
    }
    const want = input.checked;
    this.toggling = true;
    // aria-disabled too: screen readers ignore aria-busy on a checkbox, and a
    // flip made now is undone (see above).
    input.setAttribute("aria-busy", "true");
    input.setAttribute("aria-disabled", "true");
    try {
      const res = await api.patchConfig({ updates: { check: want } });
      store.applyConfig(res.config);
      // The appliance applied the change before answering. Record it in the
      // update state too, which drops system reads started before it, so
      // neither such a read nor a failed refresh flips the switch back.
      const cur = store.getState().system?.update;
      if (cur) store.applyUpdateStatus(withChecksSetting(cur, want));
      showToast(want ? "Daily update check turned on." : "Daily update check turned off.");
    } catch (err: unknown) {
      if (isRefusal(err)) {
        input.checked = !want;
        showToast(`Could not change the update check: ${apiErrorMessage(err)}`, "error");
      } else {
        // It may have applied: the refresh below sets the switch from the
        // appliance.
        showUnconfirmed("that the update check was changed", "refreshing");
      }
    } finally {
      this.toggling = false;
      input.removeAttribute("aria-busy");
      input.removeAttribute("aria-disabled");
    }
    await store.refreshSystem();
    this.render();
  }

  private async checkNow(): Promise<void> {
    const btn = this.checkBtn;
    if (!btn || this.checking || btn.getAttribute("aria-disabled") === "true") return;
    this.checking = true;
    setBusy(btn, "Checking...");
    try {
      const status = await api.checkForUpdate();
      store.applyUpdateStatus(status);
      // Checks turned off while it ran (here or in another tab) stop a check
      // without a result; an old error must not read as this check's.
      if (!status.checkEnabled) showToast("The check stopped because update checks were turned off.", "warn");
      else if (status.lastError) showToast(`Update check failed: ${status.lastError.trim()}`, "warn");
      else if (!status.available && status.latestVersion) showToast(`Up to date: ${status.currentVersion} is the newest release.`);
    } catch (err: unknown) {
      // Any failure reads the same: a check changes no setting, so a lost
      // answer only hides a result the next status read brings.
      showToast(`Update check failed: ${failureReason(err)}`, "error");
    } finally {
      this.checking = false;
      clearBusy(btn, "Check Now");
    }
    this.render();
  }

  private async start(): Promise<void> {
    const btn = this.applyBtn;
    const u = this.update;
    const target = u ? describeUpdate(u).applyVersion : "";
    if (!btn || !u || !target || this.applying || btn.getAttribute("aria-disabled") === "true") return;
    const ok = await confirmDialog({
      title: `Update to ${target}?`,
      body: `The appliance downloads ${target}, checks its signature, and restarts to install it, which drops connected streams for a moment. If the new version does not start, it goes back to ${u.currentVersion} on its own.`,
      confirmLabel: "Update",
    });
    if (!ok) return;
    this.applying = true;
    setBusy(btn, "Starting...");
    try {
      const status = await api.startUpdate();
      // Follow from the version the answer reports, not the one read before
      // the confirm: another tab's update may have landed in between.
      this.startFollow(status.currentVersion);
      store.applyUpdateStatus(status);
    } catch (err: unknown) {
      // The request may have reached the appliance with only the answer lost,
      // or another tab may have started an update: read the state again and
      // follow an update that is under way.
      const now = (await store.refreshSystem()) ? this.update : undefined;
      if (now && updateUnderway(now.phase)) {
        showToast("An update is already under way; following it here.", "warn");
        this.startFollow(now.currentVersion);
        this.followStatus(now);
      } else if (isRefusal(err)) {
        showToast(`Update did not start: ${apiErrorMessage(err)}`, "error");
      } else if (now) {
        showUnconfirmed("that the update started", "no update is under way yet; try again if none begins");
      } else {
        showUnconfirmed("that the update started", "its status could not be read either; check it before trying again");
      }
    } finally {
      this.applying = false;
    }
    this.render();
  }
}
