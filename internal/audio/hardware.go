package audio

import (
	"slices"
	"strings"
)

// candidateRates is the set of sample rates probed at startup and offered in the
// config UI: the common ALSA capture rates from 16 kHz to the 384 kHz ultrasonic
// ceiling. ProbeRates keeps the subset a given device actually accepts.
//
// Keep this in sync with STANDARD_RATES in web/src/components/device-settings.ts,
// the frontend fallback used when a device reports no probed rates.
var candidateRates = []int{16000, 22050, 32000, 44100, 48000, 88200, 96000, 176400, 192000, 256000, 384000}

// CandidateRates returns a copy of the startup probe candidate rates. It returns
// a fresh slice so a caller cannot mutate the shared package list.
func CandidateRates() []int { return slices.Clone(candidateRates) }

// candidateChannels is the set of channel counts probed at startup and offered in
// the config UI. config.Validate accepts only 1 or 2 (Opus needs mono; PCM
// supports up to stereo), so those are the only counts worth probing. ProbeChannels
// keeps the subset a given device actually accepts.
var candidateChannels = []int{1, 2}

// CandidateChannels returns a copy of the startup probe candidate channel counts.
// It returns a fresh slice so a caller cannot mutate the shared package list.
func CandidateChannels() []int { return slices.Clone(candidateChannels) }

// DetectedDevice is one capture device the host exposes, with the capabilities
// probed for it. It is what the web UI lists as an "available" device the
// operator can enable, so it carries no configuration (name, path, mode): those
// are derived when the device is provisioned. Empty SupportedRates/Channels mean
// the device could not be probed (busy or gone) and the UI falls back to a
// static list.
type DetectedDevice struct {
	ID                string
	FriendlyName      string
	SupportedRates    []int
	SupportedChannels []int
}

// FriendlyName derives a short, human-facing device label from an ALSA card
// name by keeping the text before the first comma and trimming surrounding
// whitespace. ALSA longnames commonly append a bus suffix after a comma (for
// example "Scarlett 2i2 USB, USB Audio"), which is noise for a label. A name
// with no comma is returned trimmed as-is; a name that is empty before the
// first comma yields an empty string so the caller can fall back.
func FriendlyName(name string) string {
	if i := strings.IndexByte(name, ','); i >= 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}
