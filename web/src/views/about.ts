// AboutView renders the About page (#/about): what the appliance is and its
// build version, a sponsorship request, where to report bugs and ask questions,
// and the licenses of remote-mic and of everything it ships. The license texts come
// from licenses.json, which tools/licensegen writes from the build's module
// graph; it is a static file (served without the token), loaded the first time
// the page is shown, so an appliance nobody opens About on never fetches it.
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
} from "../lib/about-core.js";
import { router } from "../lib/router.js";
import { store } from "../lib/store.js";
import type { ApplianceStatus, SystemInfo } from "../lib/types.js";
import { button, copyText, elem, externalLink, ICON_COPY, iconSpan, renderLoadError, setText } from "../lib/ui.js";

// Section and link icons: static, trusted markup.
const svg = (body: string, size = 16): string =>
  `<svg width="${size}" height="${size}" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">${body}</svg>`;
const ICON_INFO = svg('<circle cx="12" cy="12" r="10"></circle><path d="M12 16v-4"></path><path d="M12 8h.01"></path>');
const ICON_HELP = svg('<circle cx="12" cy="12" r="10"></circle><path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3"></path><path d="M12 17h.01"></path>');
const ICON_HEART = svg('<path d="M19 14c1.49-1.46 3-3.21 3-5.5A5.5 5.5 0 0 0 16.5 3c-1.76 0-3 .5-4.5 2-1.5-1.5-2.74-2-4.5-2A5.5 5.5 0 0 0 2 8.5c0 2.3 1.5 4.05 3 5.5l7 7Z"></path>');
const ICON_SCALE = svg('<path d="m16 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1Z"></path><path d="m2 16 3-8 3 8c-.87.65-1.92 1-3 1s-2.13-.35-3-1Z"></path><path d="M7 21h10"></path><path d="M12 3v18"></path><path d="M3 7h2c2 0 5-1 7-2 2 1 5 2 7 2h2"></path>');
const ICON_BUG = svg('<path d="m8 2 1.88 1.88"></path><path d="M14.12 3.88 16 2"></path><path d="M9 7.13v-1a3.003 3.003 0 1 1 6 0v1"></path><path d="M12 20c-3.3 0-6-2.7-6-6v-3a4 4 0 0 1 4-4h4a4 4 0 0 1 4 4v3c0 3.3-2.7 6-6 6"></path><path d="M12 20v-9"></path><path d="M6.53 9C4.6 8.8 3 7.1 3 5"></path><path d="M6 13H2"></path><path d="M3 21c0-2.1 1.7-3.9 3.8-4"></path><path d="M20.97 5c0 2.1-1.6 3.8-3.5 4"></path><path d="M22 13h-4"></path><path d="M17.2 17c2.1.1 3.8 1.9 3.8 4"></path>', 12);
const ICON_CHAT = svg('<path d="M7.9 20A9 9 0 1 0 4 16.1L2 22Z"></path>', 12);
const ICON_HEART_SM = svg('<path d="M19 14c1.49-1.46 3-3.21 3-5.5A5.5 5.5 0 0 0 16.5 3c-1.76 0-3 .5-4.5 2-1.5-1.5-2.74-2-4.5-2A5.5 5.5 0 0 0 2 8.5c0 2.3 1.5 4.05 3 5.5l7 7Z"></path>', 12);

const LOADING_TEXT = "Loading license texts...";
const LICENSES_TIMEOUT_MS = 15_000;

// section builds a card in the System view's style: an icon heading, a one-line
// description, and a body the caller fills.
function section(icon: string, title: string, desc: string): { card: HTMLElement; body: HTMLElement } {
  const card = elem("section", "config-section-card about-card");
  const head = elem("div", "section-head");
  const titles = elem("div");
  const h = elem("h2", "section-title");
  h.appendChild(iconSpan(icon));
  h.appendChild(elem("span", undefined, title));
  titles.append(h, elem("span", "section-desc", desc));
  head.appendChild(titles);
  const body = elem("div", "about-body");
  card.append(head, body);
  return { card, body };
}

// licenseTextBlock renders one license file's text as a scrolling block. It is
// a focusable, labelled region so a keyboard user can scroll it in every
// browser (Safari does not make an overflowing element focusable by itself).
function licenseTextBlock(label: string, text: string): HTMLElement {
  const pre = elem("pre", "license-text mono", text);
  pre.tabIndex = 0;
  pre.setAttribute("role", "region");
  pre.setAttribute("aria-label", label);
  return pre;
}

// licenseText renders one license file as a disclosure, closed by default
// (the texts run to thousands of lines together): summary is its visible line,
// label names the text region for assistive tech.
function licenseText(summary: string, label: string, text: string): HTMLElement {
  const d = elem("details", "license-text-details");
  d.appendChild(elem("summary", "license-text-summary", summary));
  d.appendChild(licenseTextBlock(label, text));
  return d;
}

export class AboutView {
  private versionEl: HTMLElement | null = null;
  private detailsEl: HTMLElement | null = null;
  private projectLicenseEl: HTMLElement | null = null;
  private thirdPartyEl: HTMLElement | null = null;
  private status: ApplianceStatus | null = null;
  private system: SystemInfo | null = null;
  // idle until the first visit; loading while the fetch runs; done once rendered.
  // A failure returns to idle so Retry (or the next visit) tries again.
  private licenses: "idle" | "loading" | "done" = "idle";

  constructor() {
    const root = document.getElementById("view-about");
    if (!root) return;
    const { status, system } = store.getState();
    this.status = status;
    this.system = system;
    root.appendChild(this.build());
    this.renderLive();

    store.addEventListener("status", (e: Event) => {
      this.status = (e as CustomEvent<ApplianceStatus>).detail;
      this.renderLive();
    });
    store.addEventListener("system", (e: Event) => {
      this.system = (e as CustomEvent<SystemInfo>).detail;
      this.renderLive();
    });
    router.addEventListener("route", (e: Event) => {
      if ((e as CustomEvent<string>).detail === "about") void this.loadLicenses();
    });
  }

  private build(): HTMLElement {
    const stack = elem("div", "config-layout");
    stack.append(this.buildProject(), this.buildSupport(), this.buildHelp(), this.buildLicense());
    return stack;
  }

  private buildProject(): HTMLElement {
    const { card, body } = section(ICON_INFO, "About Remote Mic", "A remote microphone streaming appliance for BirdNET-Go.");
    body.appendChild(elem(
      "p",
      "about-text",
      "Remote Mic turns a small Linux board and a USB microphone or sound card into a remote microphone for BirdNET-Go. It captures audio from the board's sound devices and streams it over RTSP, as Opus or as lossless PCM up to ultrasonic sample rates, and announces itself on the local network so BirdNET-Go can find it.",
    ));
    const dl = elem("dl", "info-grid");
    const row = (key: string, val: HTMLElement): void => {
      dl.appendChild(elem("dt", "info-key", key));
      const dd = elem("dd", "info-val");
      dd.appendChild(val);
      dl.appendChild(dd);
    };
    this.versionEl = elem("span", "mono", "-");
    row("Version", this.versionEl);
    row("Author", externalLink(AUTHOR_URL, "Tomi P. Hakala"));
    row("Source Code", externalLink(REPO_URL, "github.com/tphakala/birdnet-go-remote-mic"));
    row("License", elem("span", undefined, "MIT"));
    body.appendChild(dl);
    return card;
  }

  private buildHelp(): HTMLElement {
    const { card, body } = section(ICON_HELP, "Get Help", "Bug reports and questions both go to the project on GitHub.");
    const cols = elem("div", "about-help-cols");

    const bug = elem("div", "about-help-col");
    bug.appendChild(elem("h3", "about-subtitle", "Report a Bug"));
    bug.appendChild(elem("p", "about-text", "Something not working as it should? Search the existing issues first, and open a new one if nobody has reported it yet. A good report includes:"));
    const ul = elem("ul", "about-list");
    ul.appendChild(elem("li", undefined, "The version and host details below (Copy System Details puts them on the clipboard)."));
    ul.appendChild(elem("li", undefined, "Your microphone or sound card model."));
    ul.appendChild(elem("li", undefined, "What you did, what you expected, and what happened instead."));
    const logs = elem("li", undefined, "The log from around the time it happened: ");
    logs.appendChild(elem("code", undefined, "sudo journalctl -u remote-mic --since \"1 hour ago\""));
    ul.appendChild(logs);
    bug.appendChild(ul);
    bug.appendChild(elem("p", "about-note", "Issues are public. Remove your access token, addresses, and anything else private from logs and settings before you post them."));
    bug.appendChild(externalLink(ISSUES_URL, "Report a Bug", { className: "btn btn-secondary", icon: ICON_BUG }));

    const ask = elem("div", "about-help-col");
    ask.appendChild(elem("h3", "about-subtitle", "Ask a Question"));
    ask.appendChild(elem("p", "about-text", "Setup questions, ideas for new features, and stories from your own recordings belong in GitHub Discussions, where other users can join in too."));
    ask.appendChild(externalLink(DISCUSSIONS_URL, "Open Discussions", { className: "btn btn-secondary", icon: ICON_CHAT }));

    cols.append(bug, ask);
    body.appendChild(cols);

    const detailsHead = elem("h3", "about-subtitle", "System Details");
    this.detailsEl = elem("pre", "about-details mono");
    const copy = button({ variant: "secondary", label: "Copy System Details", icon: ICON_COPY });
    copy.addEventListener("click", () => copyText(supportDetails(this.status, this.system), "System details copied"));
    const actions = elem("div", "network-actions");
    actions.append(elem("span", "network-actions-spacer"), copy);
    body.append(detailsHead, this.detailsEl, actions);
    return card;
  }

  private buildSupport(): HTMLElement {
    const { card, body } = section(ICON_HEART, "Support the Project", "Remote Mic and BirdNET-Go are free and open source.");
    body.appendChild(elem(
      "p",
      "about-text",
      "I build and maintain Remote Mic and BirdNET-Go in my spare time. If you find them valuable, whether they help you hear more of the birds and bats around you or feed your own research, please consider sponsoring the work on GitHub. Sponsorship pays for test hardware and the hours that keep both projects moving.",
    ));
    const more = elem("p", "about-text", "New to BirdNET-Go? It is the bird sound identification app this appliance streams to: ");
    more.appendChild(externalLink(BIRDNET_GO_URL, "github.com/tphakala/birdnet-go"));
    more.appendChild(document.createTextNode("."));
    body.appendChild(more);
    const actions = elem("div", "about-actions");
    actions.appendChild(externalLink(SPONSOR_URL, "Sponsor on GitHub", { className: "btn btn-primary", icon: ICON_HEART_SM }));
    body.appendChild(actions);
    return card;
  }

  // buildLicense is one card for every license: remote-mic's own MIT license,
  // then the third-party components the build links, each with its full text.
  private buildLicense(): HTMLElement {
    const { card, body } = section(
      ICON_SCALE,
      "Licenses",
      "Remote Mic is open source under the MIT License, and built on open source components under their own licenses.",
    );
    const p = elem("p", "about-text", "You may use, copy, modify, and distribute Remote Mic under the MIT License's terms. ");
    p.appendChild(externalLink(`${REPO_URL}/blob/main/LICENSE`, "Read it on GitHub"));
    body.appendChild(p);
    // The full text arrives with licenses.json; until then the link above covers it.
    this.projectLicenseEl = elem("div");
    body.appendChild(this.projectLicenseEl);

    body.appendChild(elem("h3", "about-subtitle", "Third-Party Components"));
    body.appendChild(elem("p", "about-text", "The list comes from the modules linked into this build."));
    this.thirdPartyEl = elem("div", "about-third-party", LOADING_TEXT);
    body.appendChild(this.thirdPartyEl);
    return card;
  }

  // renderLive refreshes what follows the store: the version and the system
  // details. setText writes only on change, so a status tick costs nothing.
  private renderLive(): void {
    if (this.versionEl) setText(this.versionEl, this.status?.version || "-");
    if (this.detailsEl) setText(this.detailsEl, supportDetails(this.status, this.system));
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
    try {
      // A deadline, so a stalled request ends in the error and Retry rather
      // than "Loading" forever.
      // (AbortSignal.timeout is missing before Safari 16, which then waits.)
      const signal = typeof AbortSignal.timeout === "function" ? AbortSignal.timeout(LICENSES_TIMEOUT_MS) : undefined;
      const res = await fetch(LICENSES_PATH, { signal });
      if (res.ok) doc = parseLicenseDoc(await res.json());
    } catch {
      /* a network error or a body that is not JSON: reported below */
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
    el.textContent = "";
    el.appendChild(elem("p", "about-text", `${doc.components.length} components. Open one to read its full license text.`));
    const list = elem("ul", "license-list");
    for (const c of doc.components) list.appendChild(this.componentItem(c));
    el.appendChild(list);
  }

  private componentItem(c: LicenseEntry): HTMLElement {
    const li = elem("li", "license-item");
    const d = elem("details", "license-details");
    const summary = elem("summary", "license-summary");
    summary.appendChild(elem("span", "license-name mono", componentTitle(c)));
    summary.appendChild(elem("span", "tech-tag license-tag", c.license));
    d.appendChild(summary);
    for (const f of c.files) {
      const file = elem("div", "license-file");
      file.appendChild(elem("p", "license-file-name mono", f.name));
      file.appendChild(licenseTextBlock(`${componentTitle(c)} ${f.name}`, f.text));
      d.appendChild(file);
    }
    li.appendChild(d);
    return li;
  }
}
