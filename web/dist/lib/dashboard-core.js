// Pure, DOM-free helpers for the dashboard view, split out so they can be unit
// tested with node:test (see web/test/dashboard-core.test.ts) without a DOM.
// channelLabel renders a streamed channel selection, e.g. "Ch 1", "Ch 1+2", or
// "Ch 1+3" for a non-contiguous pair. An empty selection renders nothing.
export function channelLabel(channels) {
    if (!channels.length)
        return "";
    if (channels.length === 1)
        return `Ch ${channels[0]}`;
    return "Ch " + channels.join("+");
}
// tallyStates maps the streamed channel numbers (1-based) to a per-row on/off
// flag for a device's meter console, which has one row per captured hardware
// channel indexed from zero. Row i (hardware channel i+1) is on when a stream
// carries that channel, so the tally lights the streamed channels and the idle
// captured channels stay dim.
export function tallyStates(streamed, rowCount) {
    const set = new Set(streamed);
    const out = [];
    for (let i = 0; i < rowCount; i++)
        out.push(set.has(i + 1));
    return out;
}
