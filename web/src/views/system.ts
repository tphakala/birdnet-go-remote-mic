import { store } from "../lib/store.ts";
import { router } from "../lib/router.ts";
import { triggerApplianceRestart } from "../components/restart-modal.ts";
import { showToast } from "../components/toast.ts";
import type { ApplianceStatus, SystemInfo } from "../lib/types.ts";
import { AccessCard } from "./system/access-card.ts";
import { CertificateCard } from "./system/certificate-card.ts";
import { InfoCard } from "./system/info-card.ts";
import { NetworkCard } from "./system/network-card.ts";
import { NotificationsCard } from "./system/notifications-card.ts";
import { StreamStatus } from "./system/stream-status.ts";
import { TelemetryTiles } from "./system/telemetry-tiles.ts";
import { UpdatePanel } from "./system/update-panel.ts";

// SystemView is the System page. Each card is a class in views/system/ bound
// to the card's static markup in index.html; the view holds the store and
// router subscriptions and decides which cards an event touches.
export class SystemView {
  private system: SystemInfo | null = null;
  private status: ApplianceStatus | null = null;

  private readonly tiles: TelemetryTiles;
  private readonly info: InfoCard;
  private readonly update: UpdatePanel;
  private readonly streams: StreamStatus;
  private readonly network: NetworkCard;
  private readonly access: AccessCard;
  private readonly certificate: CertificateCard;
  private readonly notifications: NotificationsCard;

  constructor() {
    this.tiles = new TelemetryTiles(document.getElementById("sys-tiles"));
    const infoCard = document.getElementById("sys-info-card");
    this.info = new InfoCard(infoCard);
    this.update = new UpdatePanel(infoCard);
    this.streams = new StreamStatus(document.getElementById("sys-device-rows"));
    this.network = new NetworkCard(document.getElementById("sys-network-card"));
    this.access = new AccessCard(document.getElementById("sys-auth-card"));
    this.certificate = new CertificateCard(document.getElementById("sys-cert-card"));
    this.notifications = new NotificationsCard(document.getElementById("sys-notifications-card"));
    document.getElementById("btn-sys-restart")?.addEventListener("click", () => triggerApplianceRestart());

    store.on("system", (system) => {
      this.system = system;
      this.tiles.render(system);
      this.info.render(this.system, this.status);
      this.update.system(system.update);
    });
    store.on("status", (status) => {
      this.status = status;
      this.update.version(status.version, status.uptimeSeconds);
      if (this.system) this.tiles.render(this.system);
      this.info.render(this.system, this.status);
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
      this.streams.devices(devices, store.getState().config);
    });
    store.on("config", (cfg) => {
      this.network.config(cfg);
      this.access.config(cfg);
      this.notifications.config(cfg);
      this.streams.config(cfg, store.getState().devices);
    });
    store.on("loaderror", (failure) => {
      if (failure.systemFailed) this.tiles.loadError(failure.message);
      if (failure.coreFailed) this.streams.loadFailed();
      // A config-only failure leaves the network/access/notification cards
      // loading with no other signal. Surface it so the miss is not invisible;
      // polling recovers the config on a later tick and fills the cards.
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
  }
}
