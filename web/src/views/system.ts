import { api, apiErrorMessage, failureReason, isRefusal } from "../lib/api.ts";
import { store } from "../lib/store.ts";
import { router } from "../lib/router.ts";
import { clearBusy, deviceStateBadge, elem, externalLink, formatRelative, formatUptime, ICON_VERSION, iconSpan, modeLabel, orderChildren, renderLoadError, setBusy, setHidden, showUnconfirmed, setText, svgIcon } from "../lib/ui.ts";
import { confirmDialog } from "../lib/modal.ts";
import { deviceIdTitle } from "../lib/text.ts";
import { showUpdateModal, triggerApplianceRestart, type UpdateModal } from "../components/restart-modal.ts";
import { describeUpdate, followEndText, lastCheckText, safeNotesUrl, TickGuard, UpdateFollow, updateUnderway, VersionWatch, withChecksSetting } from "../lib/update-core.ts";
import { showToast } from "../components/toast.ts";
import type { ApplianceStatus, Device, SystemInfo, UpdateStatus } from "../lib/types.ts";
import { captureFormatLabel, clientSummary, streamSummary } from "../lib/dashboard-core.ts";
import { AccessCard } from "./system/access-card.ts";
import { CertificateCard } from "./system/certificate-card.ts";
import { NetworkCard } from "./system/network-card.ts";
import { NotificationsCard } from "./system/notifications-card.ts";

// System Information item icons (Lucide glyphs), one per label. The card splits
// into a Hardware column (physical machine) and a Software column (OS + build).
const ICON_PLATFORM =
  svgIcon('<polygon points="12 2 2 7 12 12 22 7 12 2"></polygon><polyline points="2 17 12 22 22 17"></polyline><polyline points="2 12 12 17 22 12"></polyline>', 14);
const ICON_CPU =
  svgIcon('<rect width="16" height="16" x="4" y="4" rx="2"></rect><rect width="6" height="6" x="9" y="9" rx="1"></rect><path d="M15 2v2"></path><path d="M15 20v2"></path><path d="M2 15h2"></path><path d="M2 9h2"></path><path d="M20 15h2"></path><path d="M20 9h2"></path><path d="M9 2v2"></path><path d="M9 20v2"></path>', 14);
const ICON_MEMORY =
  svgIcon('<rect x="3" y="8" width="18" height="8" rx="1"></rect><path d="M6 16v2"></path><path d="M10 16v2"></path><path d="M14 16v2"></path><path d="M18 16v2"></path>', 14);
const ICON_STORAGE =
  svgIcon('<line x1="22" x2="2" y1="12" y2="12"></line><path d="M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"></path><line x1="6" x2="6.01" y1="16" y2="16"></line><line x1="10" x2="10.01" y1="16" y2="16"></line>', 14);
const ICON_HOST =
  svgIcon('<rect width="20" height="8" x="2" y="2" rx="2" ry="2"></rect><rect width="20" height="8" x="2" y="14" rx="2" ry="2"></rect><line x1="6" x2="6.01" y1="6" y2="6"></line><line x1="6" x2="6.01" y1="18" y2="18"></line>', 14);
const ICON_OS =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><circle cx="12" cy="12" r="2"></circle>', 14);
const ICON_KERNEL =
  svgIcon('<polyline points="4 17 10 11 4 5"></polyline><line x1="12" x2="20" y1="19" y2="19"></line>', 14);
const ICON_CLOCK =
  svgIcon('<circle cx="12" cy="12" r="10"></circle><polyline points="12 6 12 12 16 14"></polyline>', 14);
// Release rows in the Software column.
const ICON_RELEASE =
  svgIcon('<path d="M11 21.73a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73z"></path><path d="M12 22V12"></path><polyline points="3.29 7 12 12 20.71 7"></polyline><path d="m7.5 4.27 9 5.15"></path>', 14);
const ICON_HISTORY =
  svgIcon('<path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8"></path><path d="M3 3v5h5"></path><path d="M12 7v5l4 2"></path>', 14);

// InfoRow is one System Information line: which column it belongs to, its label,
// the leading icon markup, and the value string.
interface InfoRow {
  group: "hw" | "sw";
  label: string;
  icon: string;
  value: string;
}

// formatByteSize renders a byte count as GB (>= 1 GB) or MB, for the static
// Memory and Storage totals in System Information. The live usage of each is in
// the telemetry tiles above.
function formatByteSize(bytes: number): string {
  const gb = bytes / 1073741824;
  if (gb >= 1) return `${gb >= 10 ? Math.round(gb) : gb.toFixed(1)} GB`;
  return `${Math.round(bytes / 1048576)} MB`;
}

// VERSION_NOTICE_MS keeps the "reload onto the new version" notice up long
// enough to be seen by someone who comes back to the tab.
const VERSION_NOTICE_MS = 5 * 60_000;

// TileSpec is one resource-gauge tile's data for a render pass; TileRefs are the
// stable nodes a tile reuses across polls so renderTiles updates in place rather
// than rebuilding the grid. barPct undefined means the tile shows no progress bar.
interface TileSpec {
  key: string;
  label: string;
  sub: string;
  value: string;
  unit: string;
  barPct: number | undefined;
}
interface TileRefs {
  tile: HTMLElement;
  sub: HTMLElement;
  value: HTMLElement;
  unit: HTMLElement;
  bar: HTMLElement;
  barFill: HTMLElement;
}

// DeviceRowRefs are the stable cells of one Stream-Status table row, reused
// across polls so renderDeviceRows updates text in place instead of rebuilding
// every row each tick.
interface DeviceRowRefs {
  tr: HTMLElement;
  name: HTMLElement;
  alsa: HTMLElement;
  path: HTMLElement;
  codec: HTMLElement;
  client: HTMLElement;
  stateSpan: HTMLElement;
}

export class SystemView {
  private tilesEl: HTMLElement | null;
  private infoHwEl: HTMLElement | null;
  private infoSwEl: HTMLElement | null;
  private infoCardEl: HTMLElement | null;
  private rowsEl: HTMLElement | null;
  private system: SystemInfo | null = null;
  private status: ApplianceStatus | null = null;
  // Stable per-poll nodes for the diffed telemetry renders: resource tiles keyed
  // by tile key, System-Information rows keyed by label, and Stream-Status rows
  // keyed by device id. Built once, updated in place, added/removed on change.
  private tileEls = new Map<string, TileRefs>();
  private infoRows = new Map<string, { dt: HTMLElement; dd: HTMLElement }>();
  private deviceRows = new Map<string, DeviceRowRefs>();
  private deviceEmptyRow: HTMLElement | null = null;
  private devicesLoaded = false;

  private readonly network: NetworkCard;
  private readonly access: AccessCard;
  private readonly certificate: CertificateCard;
  private readonly notifications: NotificationsCard;


  private updateCheckEl: HTMLInputElement | null;
  private updateCheckBtn: HTMLElement | null;
  private updateApplyBtn: HTMLElement | null;
  // The release notes address the footer's link was built for.
  private updateNotesUrl = "";
  // Requests in flight from this card; a render never undoes their busy state.
  private updateChecking = false;
  private updateApplying = false;
  private updateToggling = false;
  // The update this tab started, followed from the request until it ends or
  // the page reloads onto the new version; null when not following.
  private follow: UpdateFollow | null = null;
  private updateModal: UpdateModal | null = null;
  private followTimer: ReturnType<typeof setInterval> | null = null;
  // The version this page's scripts came from, and the last new version this
  // tab was told about, so the reload notice shows once per version.
  private readonly versionWatch = new VersionWatch();
  private versionNoticeFor = "";
  private reloading = false;

  constructor() {
    this.tilesEl = document.getElementById("sys-tiles");
    this.infoHwEl = document.getElementById("sys-info-hw");
    this.infoSwEl = document.getElementById("sys-info-sw");
    this.infoCardEl = document.getElementById("sys-info-card");
    this.rowsEl = document.getElementById("sys-device-rows");
    this.network = new NetworkCard(document.getElementById("sys-network-card"));
    this.access = new AccessCard(document.getElementById("sys-auth-card"));
    this.certificate = new CertificateCard(document.getElementById("sys-cert-card"));
    this.notifications = new NotificationsCard(document.getElementById("sys-notifications-card"));
    this.updateCheckEl = document.getElementById("sys-update-check") as HTMLInputElement | null;
    this.updateCheckBtn = document.getElementById("btn-update-check");
    this.updateApplyBtn = document.getElementById("btn-update-apply");
    const btn = document.getElementById("btn-sys-restart") as HTMLButtonElement | null;
    if (btn) btn.addEventListener("click", () => triggerApplianceRestart());

    store.on("system", (system) => {
      this.system = system;
      this.renderTiles();
      this.renderInfo();
      this.renderUpdate();
    });
    store.on("status", (status) => {
      this.status = status;
      this.watchVersion(this.status.version, this.status.uptimeSeconds);
      this.renderTiles();
      this.renderInfo();
      this.network.overrides(status.overrides);
      // While the System view is showing, each status event refreshes the
      // certificate metadata so the card does not go stale after a
      // regenerate, an install, or a change made outside this page; after a
      // 501 the card stops polling the endpoint. The store announces
      // status only on change, but uptimeSeconds advances between polls, so
      // this runs every tick while the view is showing (the store pauses
      // polling while the page is hidden). Off the view nobody reads the card;
      // the route listener below loads it on arrival.
      if (router.getCurrentView() === "system") this.certificate.load();
    });
    // Arriving on the System view loads the certificate at once rather than on
    // the next status tick. Only once a status event has arrived: that is when
    // access (the token, if any) is settled, so an early route event at boot
    // does not send a request the login prompt would have to absorb.
    router.on("route", (view) => {
      if (view === "system" && this.status !== null) this.certificate.load();
    });
    store.on("devices", (devices) => {
      this.devicesLoaded = true;
      this.renderDeviceRows(devices);
    });
    store.on("config", (cfg) => {
      this.network.config(cfg);
      this.access.config(cfg);
      this.notifications.config(cfg);
      // The table lists every configured stream, which only the config holds.
      // config fires on every poll, but the rows write only what changed.
      // Only once the devices loaded, or a config arriving first would flash
      // "No devices configured".
      if (this.devicesLoaded) this.renderDeviceRows(store.getState().devices);
    });
    store.on("loaderror", (failure) => {
      if (failure.systemFailed) this.renderLoadError(failure.message);
      // A config-only failure leaves the network/access/notification cards hidden
      // with no other signal. Surface it so the miss is not invisible; polling
      // recovers the config on a later tick and the cards then appear.
      if (failure.configFailed && !failure.systemFailed) {
        showToast("Could not load the network, access and notification settings. Retrying shortly.", "warn");
      }
    });
    // The open-access banner links to the System view; once it is shown, bring
    // the Access Control card into view and focus its token field so a keyboard
    // user lands on the action rather than at the top of the page.
    document.querySelector<HTMLAnchorElement>("#open-access-banner a")?.addEventListener("click", () => {
      requestAnimationFrame(() => this.access.focusToken());
    });
    this.bindUpdate();
  }

  // bindUpdate wires the update switch and buttons in the System Information
  // card.
  private bindUpdate(): void {
    this.updateCheckEl?.addEventListener("change", () => void this.saveUpdateCheck());
    this.updateCheckBtn?.addEventListener("click", () => void this.checkForUpdate());
    this.updateApplyBtn?.addEventListener("click", () => void this.startUpdate());
  }

  // renderUpdate patches the update status and footer of the System
  // Information card from the store's status (a check or an update request
  // merges its response there first); renderInfo adds the release rows. An
  // appliance without update support sends none and both stay hidden.
  private renderUpdate(): void {
    const u = this.system?.update;
    const state = document.getElementById("sys-update-state");
    if (state) setHidden(state, !u);
    const actions = document.getElementById("sys-update-actions");
    if (actions) setHidden(actions, !u?.supported);
    if (!u) return;
    const view = describeUpdate(u);

    const headline = document.getElementById("sys-update-headline");
    if (headline) {
      const cls = `update-headline tone-${view.tone}`;
      if (headline.className !== cls) headline.className = cls;
      setText(headline, view.headline);
      setHidden(headline, view.headline === "");
    }
    const detail = document.getElementById("sys-update-detail");
    if (detail) {
      setText(detail, view.detail);
      setHidden(detail, view.detail === "");
    }
    // The status region stays in place for announcements; with nothing to
    // say it takes no room.
    document.getElementById("sys-update-state")?.classList.toggle("is-empty", view.headline === "");
    for (const [id, text] of [["sys-update-hint", view.hint], ["sys-update-note", view.note]] as const) {
      const el = document.getElementById(id);
      if (!el) continue;
      setText(el, text);
      setHidden(el, text === "");
    }

    // A build that names no release never checks, so the footer (hidden above)
    // with its switch and actions would only contradict the headline.
    if (this.updateCheckEl && !this.updateToggling) this.updateCheckEl.checked = u.checkEnabled;
    // Turning checks off stops a download (not an install already handed to
    // the root updater), so say so while one runs.
    const checkHint = document.getElementById("sys-update-check-hint");
    if (checkHint) {
      const warn = u.phase === "downloading" ? "Turning this off stops the download under way." : "";
      setText(checkHint, warn);
      setHidden(checkHint, warn === "");
    }
    this.renderUpdateNotes(u.available ? safeNotesUrl(u.notesUrl) : "");

    // Check Now stays in place (hiding a focused button would drop focus) and
    // shows as unavailable while it cannot run.
    const check = this.updateCheckBtn;
    if (check) {
      if (!this.updateChecking) {
        if (view.canCheck) check.removeAttribute("aria-disabled");
        else check.setAttribute("aria-disabled", "true");
      }
    }
    const apply = this.updateApplyBtn;
    if (apply) {
      const hide = view.applyVersion === "" && !view.busy;
      // Update can vanish under focus (checks turned off in another tab);
      // Check Now beside it keeps the keyboard in the card.
      if (hide && !apply.hidden && document.activeElement === apply) this.updateCheckBtn?.focus({ preventScroll: true });
      setHidden(apply, hide);
      if (view.busy) setBusy(apply, u.phase === "installing" ? "Installing..." : u.phase === "downloading" ? "Downloading..." : "Updating...");
      else if (!this.updateApplying && view.applyVersion) clearBusy(apply, `Update to ${view.applyVersion}`);
    }
    this.followStatus(u);
  }

  // renderUpdateNotes shows the release notes link for the offered release,
  // rebuilt only when the address changes.
  private renderUpdateNotes(url: string): void {
    const slot = document.getElementById("sys-update-notes");
    if (!slot || url === this.updateNotesUrl) return;
    this.updateNotesUrl = url;
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
    if (this.updateModal) return;
    this.updateModal = showUpdateModal(target);
    if (!this.updateModal) return;
    const shownAt = Date.now();
    this.follow?.shown(shownAt);
    // A deadline read after the page was hidden or asleep says nothing about
    // the install until a fresh status has had a chance to arrive.
    const guard = new TickGuard(shownAt);
    this.followTimer = setInterval(() => {
      const now = Date.now();
      if (!document.hidden) this.updateModal?.elapsed(now - shownAt);
      if (!guard.mayCheck(now, document.hidden)) return;
      if (this.follow?.tick(now) === "timeout") {
        this.stopFollowTimer();
        this.updateModal?.overdue();
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
    const hadModal = this.updateModal !== null;
    const reachedInstall = this.follow?.reachedInstall ?? false;
    this.stopFollowTimer();
    this.updateModal?.hide();
    this.updateModal = null;
    this.follow = null;
    const { text, tone } = followEndText(u, reachedInstall);
    showToast(text, tone);
    if (hadModal) {
      const apply = this.updateApplyBtn;
      (apply && !apply.hidden ? apply : this.updateCheckBtn)?.focus({ preventScroll: true });
    }
  }

  // watchVersion notices the appliance running another version than this page
  // loaded against. A tab following its own update shows the wait as soon as
  // the new version answers and reloads once it has settled; any other tab (or
  // one whose follow already ended) is told to reload once it has settled,
  // never reloaded under unsaved input.
  private watchVersion(version: string, uptimeSeconds: number): void {
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

  private reloadOntoNewVersion(version: string): void {
    if (this.reloading) return;
    this.reloading = true;
    this.stopFollowTimer();
    this.updateModal ??= showUpdateModal(version);
    this.updateModal?.reloading();
    setTimeout(() => window.location.reload(), 600);
  }

  private async saveUpdateCheck(): Promise<void> {
    const input = this.updateCheckEl;
    if (!input) return;
    // A flip while the last one is still saving is undone at once rather than
    // left showing a state that was never sent.
    if (this.updateToggling) {
      input.checked = !input.checked;
      return;
    }
    const want = input.checked;
    this.updateToggling = true;
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
      this.updateToggling = false;
      input.removeAttribute("aria-busy");
      input.removeAttribute("aria-disabled");
    }
    await store.refreshSystem();
    this.renderUpdate();
  }

  private async checkForUpdate(): Promise<void> {
    const btn = this.updateCheckBtn;
    if (!btn || this.updateChecking || btn.getAttribute("aria-disabled") === "true") return;
    this.updateChecking = true;
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
      this.updateChecking = false;
      clearBusy(btn, "Check Now");
    }
    this.renderUpdate();
  }

  private async startUpdate(): Promise<void> {
    const btn = this.updateApplyBtn;
    const u = this.system?.update;
    const target = u ? describeUpdate(u).applyVersion : "";
    if (!btn || !u || !target || this.updateApplying || btn.getAttribute("aria-disabled") === "true") return;
    const ok = await confirmDialog({
      title: `Update to ${target}?`,
      body: `The appliance downloads ${target}, checks its signature, and restarts to install it, which drops connected streams for a moment. If the new version does not start, it goes back to ${u.currentVersion} on its own.`,
      confirmLabel: "Update",
    });
    if (!ok) return;
    this.updateApplying = true;
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
      const now = (await store.refreshSystem()) ? this.system?.update : undefined;
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
      this.updateApplying = false;
    }
    this.renderUpdate();
  }

  // renderLoadError swaps the telemetry placeholder for the failure cause and a
  // Retry button so the system view is not stuck loading when /system is
  // unreachable. A successful retry re-renders via the system event.
  private renderLoadError(message: string): void {
    if (!this.tilesEl) return;
    this.tilesEl.textContent = "";
    // The tiles were just detached, so drop their stale refs; otherwise a later
    // renderTiles would reuse detached nodes and the diffed pass would not rebuild.
    this.tileEls.clear();
    const p = elem("p", "cfg-empty");
    this.tilesEl.appendChild(p);
    renderLoadError(p, message, "Loading system telemetry...", () => void store.retry());
  }

  // buildTile creates one resource-gauge tile with stable inner nodes (the sub,
  // value, unit and progress bar are always present and toggled/updated, never
  // rebuilt), so renderTiles can update it in place across polls. The label is
  // fixed per tile key, so it is written once here.
  private buildTile(label: string): TileRefs {
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

  private updateTile(refs: TileRefs, spec: TileSpec): void {
    setText(refs.sub, spec.sub);
    setHidden(refs.sub, !spec.sub);
    setText(refs.value, spec.value);
    setText(refs.unit, spec.unit);
    setHidden(refs.unit, !spec.unit);
    if (spec.barPct === undefined) {
      setHidden(refs.bar, true);
    } else {
      setHidden(refs.bar, false);
      const w = `${Math.min(100, Math.max(0, spec.barPct)).toFixed(1)}%`;
      if (refs.barFill.style.width !== w) refs.barFill.style.width = w;
    }
  }

  // The tile grid holds only the live resource gauges. Host and appliance facts
  // live in the separate System Information card below. Rendered with the diffed
  // convention: tiles are keyed by a stable key, updated in place, and only
  // added/removed/reordered when the present set changes.
  private renderTiles(): void {
    if (!this.tilesEl) return;
    const grid = this.tilesEl;
    const sys = this.system;
    if (!sys) return;

    // Remove a load-error placeholder renderLoadError may have left in the grid, so
    // a recovered poll does not strand it among the gauges: the diffed pass below
    // tracks only tile nodes, not this foreign child.
    grid.querySelector(":scope > .cfg-empty")?.remove();

    const specs: TileSpec[] = [];
    specs.push({
      key: "cpu", label: "CPU Utilization", sub: sys.cpuCores > 0 ? `${sys.cpuCores} Cores` : "",
      value: sys.cpuPercent !== undefined ? sys.cpuPercent.toFixed(1) : "n/a", unit: "%", barPct: sys.cpuPercent,
    });
    if (sys.memTotalBytes > 0) {
      specs.push({
        key: "mem", label: "Memory", sub: `${formatByteSize(sys.memTotalBytes)} Total`,
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
        key: "disk", label: "Disk", sub: `${formatByteSize(sys.diskTotalBytes)} Total`,
        value: (sys.diskUsedBytes / 1073741824).toFixed(1), unit: "GB used",
        barPct: (sys.diskUsedBytes / sys.diskTotalBytes) * 100,
      });
    }

    const want = new Set(specs.map((s) => s.key));
    for (const [key, refs] of this.tileEls) {
      if (!want.has(key)) { refs.tile.remove(); this.tileEls.delete(key); }
    }

    const tiles: HTMLElement[] = [];
    for (const spec of specs) {
      let refs = this.tileEls.get(spec.key);
      if (!refs) { refs = this.buildTile(spec.label); this.tileEls.set(spec.key, refs); }
      this.updateTile(refs, spec);
      tiles.push(refs.tile);
    }
    orderChildren(grid, tiles);
  }

  // renderInfo fills the System Information label/value grid, diffed: rows are
  // keyed by label, values updated in place, and dt/dd pairs added, removed and
  // ordered only on change rather than clearing the grid every poll.
  private renderInfo(): void {
    const hw = this.infoHwEl;
    const sw = this.infoSwEl;
    if (!hw || !sw) return;
    const sys = this.system;
    const st = this.status;
    if (!sys && !st) return;

    // Two labelled columns: Hardware holds the physical machine's specs, Software
    // holds the OS and appliance build. The live gauges (CPU %, memory, temp and
    // disk usage) live in the telemetry tiles above, so this card carries static
    // facts and shows the memory and storage TOTALS rather than their usage.
    const rows: InfoRow[] = [];
    if (sys) {
      rows.push({ group: "hw", label: "Platform", icon: ICON_PLATFORM, value: sys.platform || "-" });
      if (sys.cpuModel || sys.cpuCores) {
        const cpu = sys.cpuModel
          ? `${sys.cpuModel}${sys.cpuCores ? ` (${sys.cpuCores} cores)` : ""}`
          : `${sys.cpuCores} cores`;
        rows.push({ group: "hw", label: "CPU", icon: ICON_CPU, value: cpu });
      }
      if (sys.memTotalBytes > 0) rows.push({ group: "hw", label: "Memory", icon: ICON_MEMORY, value: formatByteSize(sys.memTotalBytes) });
      if (sys.diskTotalBytes > 0) rows.push({ group: "hw", label: "Storage", icon: ICON_STORAGE, value: formatByteSize(sys.diskTotalBytes) });
      rows.push({ group: "sw", label: "Hostname", icon: ICON_HOST, value: sys.hostname || "-" });
      if (sys.os) rows.push({ group: "sw", label: "OS", icon: ICON_OS, value: sys.os });
      if (sys.kernel) rows.push({ group: "sw", label: "Kernel", icon: ICON_KERNEL, value: sys.kernel });
    }
    if (st) rows.push({ group: "sw", label: "Version", icon: ICON_VERSION, value: st.version || "-" });
    // A build that names no release never checks, so it has no release rows.
    const u = sys?.update;
    if (u?.supported) {
      rows.push({ group: "sw", label: "Latest Release", icon: ICON_RELEASE, value: u.latestVersion ?? "-" });
      rows.push({ group: "sw", label: "Last Check", icon: ICON_HISTORY, value: lastCheckText(u.lastCheck, Date.now(), formatRelative) });
    }
    if (st) rows.push({ group: "sw", label: "Uptime", icon: ICON_CLOCK, value: formatUptime(st.uptimeSeconds) });

    const want = new Set(rows.map((r) => r.label));
    for (const [key, pair] of this.infoRows) {
      if (!want.has(key)) { pair.dt.remove(); pair.dd.remove(); this.infoRows.delete(key); }
    }

    // Each column is ordered within its own grid, dt then dd per row.
    const order: Record<"hw" | "sw", HTMLElement[]> = { hw: [], sw: [] };
    for (const r of rows) {
      let pair = this.infoRows.get(r.label);
      if (!pair) {
        const dt = elem("dt", "info-key");
        dt.append(iconSpan(r.icon, "info-key-icon"), document.createTextNode(r.label));
        pair = { dt, dd: elem("dd", "info-val mono", r.value) };
        this.infoRows.set(r.label, pair);
      } else {
        setText(pair.dd, r.value);
      }
      order[r.group].push(pair.dt, pair.dd);
    }
    orderChildren(hw, order.hw);
    orderChildren(sw, order.sw);
    if (this.infoCardEl) this.infoCardEl.hidden = rows.length === 0;
  }

  private buildDeviceRow(): DeviceRowRefs {
    const tr = document.createElement("tr");
    const name = this.td("");
    const alsa = this.td("", true);
    const path = elem("td", "mono stream-paths");
    const codec = this.td("", true);
    const client = this.td("", true);
    const stateTd = document.createElement("td");
    const stateSpan = elem("span");
    stateTd.appendChild(stateSpan);
    tr.append(name, alsa, path, codec, client, stateTd);
    return { tr, name, alsa, path, codec, client, stateSpan };
  }

  private updateDeviceRow(r: DeviceRowRefs, d: Device): void {
    setText(r.name, d.name);
    // The current ALSA address; when the id resolved to no single present device
    // it is absent, so a serving card-index device (container fallback) shows its
    // configured id and anything else shows "-". The persisted id is long and
    // goes in the tooltip.
    setText(r.alsa, d.hwAddr ?? (d.state === "serving" ? d.device : "-"));
    const alsaTitle = deviceIdTitle(d.device);
    if (r.alsa.title !== alsaTitle) r.alsa.title = alsaTitle;
    // By the device id, which a rename does not change.
    const streams = streamSummary(d, store.getState().config?.devices.find((c) => c.device === d.device));
    // One path per line (the cell keeps the line breaks), in stream order.
    setText(r.path, streams.paths.join("\n"));
    const rate = d.negotiatedRate ?? d.rate;
    const negFormat = d.negotiatedFormat ? ` · ${captureFormatLabel(d.negotiatedFormat)}` : "";
    setText(r.codec, `${streams.modes.map(modeLabel).join(" + ")} ${rate.toLocaleString("en-US")} Hz${negFormat}`);
    // The format is the hardware capture depth; the RTSP stream stays 16-bit.
    const codecTitle = d.negotiatedFormat
      ? "Hardware capture format. The RTSP stream is 16-bit; a wider capture is downconverted."
      : "";
    if (r.codec.title !== codecTitle) r.codec.title = codecTitle;
    setText(r.client, clientSummary(streams));
    const badge = deviceStateBadge(d.state);
    if (r.stateSpan.className !== badge.cls) r.stateSpan.className = badge.cls;
    setText(r.stateSpan, badge.label);
  }

  // renderDeviceRows fills the Stream-Status table, diffed: rows are keyed by the
  // immutable device id, cells updated in place, and rows added, removed and
  // ordered only on change rather than rebuilding the whole tbody every poll.
  private renderDeviceRows(devices: Device[]): void {
    if (!this.rowsEl) return;
    const body = this.rowsEl;

    if (devices.length === 0) {
      for (const r of this.deviceRows.values()) r.tr.remove();
      this.deviceRows.clear();
      if (!this.deviceEmptyRow) {
        const tr = document.createElement("tr");
        const td = elem("td", undefined, "No devices configured.");
        td.setAttribute("colspan", "6");
        tr.appendChild(td);
        this.deviceEmptyRow = tr;
      }
      if (this.deviceEmptyRow.parentNode !== body) body.appendChild(this.deviceEmptyRow);
      return;
    }
    // Non-empty: drop the placeholder row if it is showing.
    if (this.deviceEmptyRow?.parentNode) this.deviceEmptyRow.remove();

    const want = new Set(devices.map((d) => d.device));
    for (const [id, r] of this.deviceRows) {
      if (!want.has(id)) { r.tr.remove(); this.deviceRows.delete(id); }
    }

    const rows: HTMLElement[] = [];
    for (const d of devices) {
      let r = this.deviceRows.get(d.device);
      if (!r) { r = this.buildDeviceRow(); this.deviceRows.set(d.device, r); }
      this.updateDeviceRow(r, d);
      rows.push(r.tr);
    }
    orderChildren(body, rows);
  }

  private td(text: string, mono = false): HTMLElement {
    return elem("td", mono ? "mono" : undefined, text);
  }
}
