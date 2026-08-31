package audio

import "testing"

const (
	testDevID    = "hw:1,0"
	testCardName = "C-Media USB Audio Device"
	testFmtS16   = "s16"
)

func TestCandidateRatesReturnsFreshCopy(t *testing.T) {
	t.Parallel()
	a := CandidateRates()
	if len(a) == 0 {
		t.Fatal("CandidateRates returned an empty slice")
	}
	a[0] = -1 // mutate the returned slice
	if b := CandidateRates(); b[0] == -1 {
		t.Error("CandidateRates returned a slice aliasing the package var; a mutation leaked")
	}
}

func TestCandidateChannelsReturnsFreshCopy(t *testing.T) {
	t.Parallel()
	a := CandidateChannels()
	if len(a) != 2 || a[0] != 1 || a[1] != 2 {
		t.Fatalf("CandidateChannels = %v, want [1 2]", a)
	}
	a[0] = -1 // mutate the returned slice
	if b := CandidateChannels(); b[0] == -1 {
		t.Error("CandidateChannels returned a slice aliasing the package var; a mutation leaked")
	}
}

func TestFriendlyName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"comma suffix dropped", "Scarlett 2i2 USB, USB Audio", "Scarlett 2i2 USB"},
		{"short name comma suffix", "AMS-24, USB Audio", "AMS-24"},
		{"no comma kept as-is", testCardName, testCardName},
		{"whitespace trimmed around comma", "  Foo , Bar", "Foo"},
		{"empty stays empty", "", ""},
		{"leading comma yields empty", ", USB Audio", ""},
		{"multiple commas cut at first", "A, B, C", "A"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FriendlyName(tc.in); got != tc.want {
				t.Errorf("FriendlyName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
