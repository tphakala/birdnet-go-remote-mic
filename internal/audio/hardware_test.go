package audio

import "testing"

func TestFriendlyName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"comma suffix dropped", "Scarlett 2i2 USB, USB Audio", "Scarlett 2i2 USB"},
		{"short name comma suffix", "AMS-24, USB Audio", "AMS-24"},
		{"no comma kept as-is", "C-Media USB Audio Device", "C-Media USB Audio Device"},
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
