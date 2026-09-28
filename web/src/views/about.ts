// AboutView renders the About page (#/about): what the appliance is and its
// build version, a sponsorship request, where to report bugs and ask questions,
// and the licenses of remote-mic and of everything it ships. The license texts
// come from licenses.json, which tools/licensegen writes from the build's module
// graph; it is a static file (served without the token), loaded the first time
// the page is shown, so an appliance nobody opens About on never fetches it.
import { withDeadline } from "../lib/deadline.ts";
import {
  AUTHOR_URL,
  BIRDNET_GO_URL,
  componentTitle,
  DISCUSSIONS_URL,
  ISSUES_URL,
  LICENSES_PATH,
  parseLicenseDoc,
  REPO_URL,
  SPONSOR_URL,
  supportDetails,
  type LicenseDoc,
  type LicenseEntry,
} from "../lib/about-core.ts";
import { router } from "../lib/router.ts";
import { store } from "../lib/store.ts";
import type { ApplianceStatus, Device, DeviceConfig, SystemInfo } from "../lib/types.ts";
import { button, copyText, externalLink, h, ICON_COPY, ICON_VERSION, iconSpan, renderLoadError, setText, svgIcon } from "../lib/ui.ts";

// Section and link icons: static, trusted markup.
const ICON_INFO = svgIcon('<circle cx="12" cy="12" r="10"></circle><path d="M12 16v-4"></path><path d="M12 8h.01"></path>');
const ICON_HELP = svgIcon('<circle cx="12" cy="12" r="10"></circle><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"></path><path d="M12 17h.01"></path>');
const HEART_PATH = '<path d="M19 14c1.49-1.46 3-3.21 3-5.5A5.5 5.5 0 0 0 16.5 3c-1.76 0-3 .5-4.5 2-1.5-1.5-2.74-2-4.5-2A5.5 5.5 0 0 0 2 8.5c0 2.3 1.5 4.05 3 5.5l7 7Z"></path>';
const ICON_HEART = svgIcon(HEART_PATH);
const SCALE_PATH = '<path d="m16 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1Z"></path><path d="m2 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1Z"></path><path d="M7 21h10"></path><path d="M12 3v18"></path><path d="M3 7h2c2 0 5-1 7-2 2 1 5 2 7 2h2"></path>';
const ICON_SCALE = svgIcon(SCALE_PATH);
const ICON_BUG = svgIcon('<path d="m8 2 1.88 1.88"></path><path d="M14.12 3.88 16 2"></path><path d="M9 7.13v-1a3.003 3.003 0 1 1 6 0v1"></path><path d="M12 20c-3.3 0-6-2.7-6-6v-3a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v3c0 3.3-2.7 6-6 6"></path><path d="M12 20v-9"></path><path d="M6.53 9C4.6 8.8 3 7.1 3 5"></path><path d="M6 13H2"></path><path d="M3 21c0-2.1 1.7-3.9 3.8-4"></path><path d="M20.97 5c0 2.1-1.6 3.8-3.5 4"></path><path d="M22 13h-4"></path><path d="M17.2 17c2.1.1 3.8 1.9 3.8 4"></path>', 12);
const ICON_CHAT = svgIcon('<path d="M7.9 20A9 9 0 1 0 4 16.1L2 22Z"></path>', 12);
// Detail-row icons (14px Lucide glyphs), as in the System Information card.
const ICON_AUTHOR = svgIcon('<path d="M19 21v-2a4 4 0 0 0-4-4H9a4 4 0 0 0-4 4v2"></path><circle cx="12" cy="7" r="4"></circle>', 14);
const ICON_CODE = svgIcon('<polyline points="16 18 22 12 16 6"></polyline><polyline points="8 6 2 12 8 18"></polyline>', 14);
const ICON_SCALE_SM = svgIcon(SCALE_PATH, 14);
const ICON_HEART_SM = svgIcon(HEART_PATH, 12);

const LOADING_TEXT = "Loading license texts...";
const LICENSES_TIMEOUT_MS = 15_000;

// section builds a card in the System view's style: an icon heading, a one-line
// description, and a body the caller fills.
function section(icon: string, title: string, desc: string): { card: HTMLElement; body: HTMLElement } {
  const body = h("div", { class: "about-body" });
  const card = h("section", { class: "config-section-card about-card" },
    h("div", { class: "section-head" },
      h("div",
        h("h2", { class: "section-title" }, iconSpan(icon), h("span", title)),
        h("span", { class: "section-desc" }, desc),
      ),
    ),
    body,
  );
  return { card, body };
}

// licenseTextBlock renders one license file's text as a scrolling block. It is
// a focusable, labelled region so a keyboard user can scroll it in every
// browser (Safari does not make an overflowing element focusable by itself).
function licenseTextBlock(label: string, text: string): HTMLElement {
  return h("pre", { class: "license-text mono", tabindex: 0, role: "region", "aria-label": label }, text);
}

// licenseText renders one license file as a disclosure, closed by default
// (the texts run to thousands of lines together): summary is its visible line,
// label names the text region for assistive tech.
function licenseText(summary: string, label: string, text: string): HTMLElement {
  return h("details", { class: "license-text-details" },
    h("summary", { class: "license-text-summary" }, summary),
    licenseTextBlock(label, text),
  );
}

export class AboutView {
  private versionEl: HTMLElement | null = null;
  private detailsEl: HTMLElement | null = null;
  private projectLicenseEl: HTMLElement | null = null;
  private thirdPartyEl: HTMLElement | null = null;
  private status: ApplianceStatus | null = null;
  private system: SystemInfo | null = null;
  // Undefined until the first device list arrives, so the details leave the
  // device section out instead of claiming there are no devices. The store
  // announces the first applied list even when empty, and the view is built
  // before the store starts, so the event is the only feed needed.
  private devices: readonly Device[] | undefined;
  // Device configs carry every stream; undefined until GET /config loads.
  private configs: readonly DeviceConfig[] | undefined;
  // idle until the first visit; loading while the fetch runs; done once rendered.
  // A failure returns to idle so Retry (or the next visit) tries again.
  private licenses: "idle" | "loading" | "done" = "idle";
  // Whether the About route is shown. The system details (which sort and
  // scrub the device list) are rebuilt only then, not on every status tick of
  // a browser that sits on another page.
  private visible = false;

  constructor() {
    const root = document.getElementById("view-about");
    if (!root) return;
    const { status, system, config } = store.getState();
    this.status = status;
    this.system = system;
    this.configs = config?.devices;
    // The router's first route event, after every view is built, renders the
    // live details if About is the page shown.
    root.appendChild(this.build());

    store.on("status", (status) => {
      this.status = status;
      this.renderLive();
    });
    store.on("system", (system) => {
      this.system = system;
      this.renderLive();
    });
    store.on("devices", (devices) => {
      this.devices = devices;
      this.renderLive();
    });
    // config fires on every poll; renderLive rebuilds only while About shows,
    // and setText writes only on change.
    store.on("config", (config) => {
      this.configs = config.devices;
      this.renderLive();
    });
    router.on("route", (view) => {
      this.visible = view === "about";
      if (!this.visible) return;
      this.renderLive();
      void this.loadLicenses();
    });
  }

  private build(): HTMLElement {
    return h("div", { class: "config-layout" }, this.buildProject(), this.buildSupport(), this.buildHelp(), this.buildLicense());
  }

  private buildProject(): HTMLElement {
    const { card, body } = section(ICON_INFO, "About Remote Mic", "A remote microphone streaming appliance for BirdNET-Go.");
    const row = (icon: string, key: string, val: HTMLElement): Node[] => [
      h("dt", { class: "info-key" }, iconSpan(icon, "info-key-icon"), key),
      h("dd", { class: "info-val" }, val),
    ];
    body.append(
      h("p", { class: "about-text" },
        "Remote Mic turns a small Linux board and a USB microphone or sound card into a remote microphone for BirdNET-Go. It captures audio from the board's sound devices and streams it over RTSP, as Opus or as lossless PCM up to ultrasonic sample rates, and announces itself on the local network so BirdNET-Go can find it.",
      ),
      h("dl", { class: "info-grid" },
        ...row(ICON_VERSION, "Version", (this.versionEl = h("span", { class: "mono" }, "-"))),
        ...row(ICON_AUTHOR, "Author", externalLink(AUTHOR_URL, "Tomi P. Hakala")),
        ...row(ICON_CODE, "Source Code", externalLink(REPO_URL, "github.com/tphakala/birdnet-go-remote-mic")),
        ...row(ICON_SCALE_SM, "License", h("span", "Apache 2.0")),
      ),
    );
    return card;
  }

  private buildHelp(): HTMLElement {
    const { card, body } = section(ICON_HELP, "Get Help", "Bug reports and questions both go to the project on GitHub.");
    const copy = button({ variant: "secondary", label: "Copy System Details", icon: ICON_COPY });
    copy.addEventListener("click", () => copyText(supportDetails(this.status, this.system, this.devices, this.configs), "System details copied"));
    body.append(
      h("div", { class: "about-help-cols" },
        h("div", { class: "about-help-col" },
          h("h3", { class: "about-subtitle" }, "Report a Bug"),
          h("p", { class: "about-text" }, "Something not working as it should? Search the existing issues first, and open a new one if nobody has reported it yet. A good report includes:"),
          h("ul", { class: "about-list" },
            h("li", "The version and host details below (Copy System Details puts them on the clipboard)."),
            h("li", "Your microphone or sound card model."),
            h("li", "What you did, what you expected, and what happened instead."),
            h("li", "The log from around the time it happened: ", h("code", "sudo journalctl -u remote-mic --since \"1 hour ago\"")),
          ),
          h("p", { class: "about-note" }, "Issues are public. Remove your access token, addresses, and anything else private from logs and settings before you post them."),
          externalLink(ISSUES_URL, "Report a Bug", { className: "btn btn-secondary", icon: ICON_BUG }),
        ),
        h("div", { class: "about-help-col" },
          h("h3", { class: "about-subtitle" }, "Ask a Question"),
          h("p", { class: "about-text" }, "Setup questions, ideas for new features, and stories from your own recordings belong in GitHub Discussions, where other users can join in too."),
          externalLink(DISCUSSIONS_URL, "Open Discussions", { className: "btn btn-secondary", icon: ICON_CHAT }),
        ),
      ),
      h("h3", { class: "about-subtitle" }, "System Details"),
      (this.detailsEl = h("pre", { class: "about-details mono" })),
      h("div", { class: "network-actions" }, h("span", { class: "network-actions-spacer" }), copy),
    );
    return card;
  }

  private buildSupport(): HTMLElement {
    const { card, body } = section(ICON_HEART, "Support the Project", "Remote Mic and BirdNET-Go are free and open source.");
    body.append(
      h("p", { class: "about-text" },
        "I build and maintain Remote Mic and BirdNET-Go in my spare time. If you find them valuable, whether they help you hear more of the birds and bats around you or feed your own research, please consider sponsoring the work on GitHub. Sponsorship pays for test hardware and the hours that keep both projects moving.",
      ),
      h("p", { class: "about-text" },
        "New to BirdNET-Go? It is the bird sound identification app this appliance streams to: ",
        externalLink(BIRDNET_GO_URL, "github.com/tphakala/birdnet-go"),
        ".",
      ),
      h("div", { class: "about-actions" },
        externalLink(SPONSOR_URL, "Sponsor on GitHub", { className: "btn btn-primary", icon: ICON_HEART_SM }),
      ),
    );
    return card;
  }

  // buildLicense is one card for every license: remote-mic's own Apache 2.0 license,
  // then the third-party components the build links, each with its full text.
  private buildLicense(): HTMLElement {
    const { card, body } = section(
      ICON_SCALE,
      "Licenses",
      "Remote Mic is open source under the Apache License 2.0, and built on open source components under their own licenses.",
    );
    body.append(
      h("p", { class: "about-text" },
        "You may use, modify, and distribute Remote Mic under the terms of the Apache License 2.0. ",
        externalLink(`${REPO_URL}/blob/main/LICENSE`, "Read it on GitHub"),
      ),
      // The full text arrives with licenses.json; until then the link above covers it.
      (this.projectLicenseEl = h("div")),
      h("h3", { class: "about-subtitle" }, "Third-Party Components"),
      h("p", { class: "about-text" }, "The list comes from the modules linked into this build."),
      (this.thirdPartyEl = h("div", { class: "about-third-party" }, LOADING_TEXT)),
    );
    return card;
  }

  // renderLive refreshes what follows the store: the version and the system
  // details, while the page is shown; showing it again catches up. setText
  // writes only on change. The Copy button builds its own text on click.
  private renderLive(): void {
    if (!this.visible) return;
    if (this.versionEl) setText(this.versionEl, this.status?.version || "-");
    if (this.detailsEl) setText(this.detailsEl, supportDetails(this.status, this.system, this.devices, this.configs));
  }

  private async loadLicenses(): Promise<void> {
    const el = this.thirdPartyEl;
    if (!el || this.licenses !== "idle") return;
    this.licenses = "loading";
    // A visit after a failed load starts from the loading text, not the old
    // error and its Retry button (as Retry itself does).
    el.removeAttribute("role");
    el.textContent = LOADING_TEXT;
    let doc: LicenseDoc | null = null;
    // A deadline, so a stalled request ends in the error and Retry rather than
    // "Loading" forever.
    try {
      doc = await withDeadline(LICENSES_TIMEOUT_MS, async (signal) => {
        const res = await fetch(LICENSES_PATH, { signal });
        return res.ok ? parseLicenseDoc(await res.json()) : null;
      });
    } catch {
      /* a network error, the deadline, or a body that is not JSON: reported below */
    }
    if (!doc) {
      this.licenses = "idle";
      renderLoadError(el, "The license list could not be loaded.", LOADING_TEXT, () => void this.loadLicenses());
      return;
    }
    this.licenses = "done";
    this.renderLicenses(doc);
  }

  private renderLicenses(doc: LicenseDoc): void {
    if (this.projectLicenseEl) {
      this.projectLicenseEl.replaceChildren(...doc.project.files.map((f) => licenseText(`Full ${f.name} text`, `Remote Mic ${f.name}`, f.text)));
    }
    const el = this.thirdPartyEl;
    if (!el) return;
    el.removeAttribute("role");
    el.replaceChildren(
      h("p", { class: "about-text" }, `${doc.components.length} components. Open one to read its full license text.`),
      h("ul", { class: "license-list" }, ...doc.components.map((c) => this.componentItem(c))),
    );
  }

  private componentItem(c: LicenseEntry): HTMLElement {
    const title = componentTitle(c);
    return h("li", { class: "license-item" },
      h("details", { class: "license-details" },
        h("summary", { class: "license-summary" },
          h("span", { class: "license-name mono" }, title),
          h("span", { class: "tech-tag license-tag" }, c.license),
        ),
        ...c.files.map((f) =>
          h("div", { class: "license-file" },
            h("p", { class: "license-file-name mono" }, f.name),
            licenseTextBlock(`${title} ${f.name}`, f.text),
          ),
        ),
      ),
    );
  }
}
