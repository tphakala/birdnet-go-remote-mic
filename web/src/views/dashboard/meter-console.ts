import { VUMeter } from "../../components/vu-meter.ts";
import { channelHiddenMessage, focusFallbackRow, hiddenRows, tallyStates } from "../../lib/dashboard-core.ts";
import { announce, h, holdsFocus, setHidden } from "../../lib/ui.ts";

// The dB marks along the top of the console, shared by every row.
const SCALE_MARKS = ["-60", "-48", "-36", "-24", "-18", "-12", "-6", "-3", "0 dBFS"];

// MeterConsole is a serving device card's meter console: the shared dB scale
// plus one metering row per CAPTURED hardware channel, not per streamed one.
// The level meter is registered on the raw capture source with the negotiated
// channel count and reports a zero-based index per hardware channel, which
// indexes meters straight, so a non-contiguous stream selection still shows
// every captured channel. Every device labels each row with its 1-based
// hardware channel number ("Ch 1", "Ch 2", ...), so the dB scale lines up the
// same way regardless of channel count. The meters are decorative real-time
// visualizations updating ~10 Hz, hidden from the accessibility tree so they
// do not spam screen readers.
export class MeterConsole {
  public readonly el: HTMLElement;
  // One VU meter per captured hardware channel, indexed by zero-based channel.
  public readonly meters: VUMeter[] = [];
  // The row and clip button of each captured channel, indexed the same way.
  private readonly rows: HTMLElement[] = [];
  private readonly clips: HTMLButtonElement[] = [];

  constructor(count: number) {
    const n = Math.max(1, count);
    // Cap the stack height for high-channel interfaces so a 6-8 channel device
    // does not grow the card tall enough to push the dashboard down; the rows
    // scroll within the console instead. Most appliance devices are mono/stereo.
    this.el = h("div", { class: n > 4 ? "meter-console many-channels" : "meter-console" },
      h("div", { class: "meter-scale" },
        h("div", { class: "meter-scale-track" }, ...SCALE_MARKS.map((s) => h("span", s))),
      ),
    );
    for (let c = 0; c < n; c++) {
      const chNum = c + 1;
      const canvas = h("canvas", {
        class: "meter-canvas",
        // The live meter and its dB readout update ~10 Hz; hide them from
        // assistive tech to avoid announcement spam. The clip button stays
        // exposed.
        "aria-hidden": "true",
        width: 700,
        height: 22,
      });
      // The meter writes its readout, the waiting one first (MeterController).
      const dbReadout = h("span", { class: "db-readout mono", "aria-hidden": "true" });
      const clip = h("button", {
        class: "clip-latch-btn",
        type: "button",
        "aria-label": `Channel ${chNum} clip indicator, click to clear`,
        // The latch sees only the levels this page receives, which stop 30 s
        // after another view shows (LEVELS_GRACE_MS) and 60 s after the tab is
        // hidden (HIDDEN_STREAM_GRACE_MS).
        title: "Latches clipping seen while the dashboard is showing.",
        // Focus key so a rebuild that moves focus can restore it to the same row.
        "data-focus": `clip-${c}`,
      }, "CLIP");
      const row = h("div", { class: "meter-track-wrapper" },
        // A tally light in front of the channel number, lit while a stream
        // carries the channel (sync sets it). Shown for every device, mono
        // included, so the channel-label column is always present.
        h("span", { class: "meter-channel-label mono", "aria-hidden": "true" },
          h("span", { class: "meter-tally" }),
          h("span", `Ch ${chNum}`),
        ),
        h("div", { class: "meter-canvas-container" }, canvas),
        h("div", { class: "meter-stats" }, dbReadout, clip),
      );
      this.el.append(row);
      this.rows.push(row);
      this.clips.push(clip);
      this.meters.push(new VUMeter(canvas, dbReadout, clip));
    }
  }

  // sync marks each captured channel live when a stream carries it and, with
  // the per-device "hide inactive channels" preference, hides the rows no
  // stream carries. streamed holds 1-based channel numbers. A row hidden while
  // it holds focus (its clip button, as its channel leaves the stream or
  // hiding turns on) would drop focus to the document body, so focus goes to a
  // stable place in the same card: the first visible row's clip button
  // (hiddenRows never hides them all), else fallback; announceEl says why.
  public sync(streamed: number[], hideInactive: boolean, fallback: HTMLElement, announceEl: HTMLElement | null): void {
    const states = tallyStates(streamed, this.rows.length);
    const hidden = hiddenRows(states, hideInactive);
    let strandedRow = -1;
    this.rows.forEach((row, i) => {
      const on = states[i] ?? false;
      row.classList.toggle("ch-live", on);
      row.classList.toggle("ch-off", !on);
      const hide = hidden[i] ?? false;
      if (hide && !row.hidden && holdsFocus(row)) strandedRow = i;
      setHidden(row, hide);
      // A hidden row's meter leaves the frame loop; it redraws when shown again.
      const meter = this.meters[i];
      if (meter) { if (hide) meter.pause(); else meter.resume(); }
      const title = on ? `Channel ${i + 1}: streamed` : `Channel ${i + 1}: not streamed`;
      if (row.title !== title) row.title = title;
      // The tally light is aria-hidden, so carry its streamed/not-streamed
      // meaning on the row's own exposed control: the clip button.
      const clip = this.clips[i];
      const clipAria = `Channel ${i + 1} (${on ? "streamed" : "not streamed"}) clip indicator, click to clear`;
      if (clip && clip.getAttribute("aria-label") !== clipAria) clip.setAttribute("aria-label", clipAria);
    });
    if (strandedRow >= 0) {
      const target = focusFallbackRow(hidden);
      const next = this.clips[target];
      (next ?? fallback).focus();
      announce(announceEl, channelHiddenMessage(strandedRow + 1, next ? target + 1 : null));
    }
  }

  // clear puts every meter in its waiting state (see LevelsWatch).
  public clear(): void {
    for (const m of this.meters) m.clearLevels();
  }

  // destroy takes the meters off the shared frame loop before the console is
  // discarded.
  public destroy(): void {
    for (const m of this.meters) m.destroy();
  }
}
