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
