module github.com/tphakala/birdnet-go-remote-mic

go 1.27

require (
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	github.com/tphakala/go-audio-capture v0.0.0-00010101000000-000000000000
	gopkg.in/yaml.v3 v3.0.1
)

require golang.org/x/sys v0.47.0 // indirect

// Local development replaces: go-audio-capture is not yet published, and the
// go-audio-stream send primitives live on a feature branch. Remove before the
// first release.
replace (
	github.com/tphakala/go-audio-capture => ../go-audio-capture
	github.com/tphakala/go-audio-stream => ../go-audio-stream
	github.com/tphakala/go-opus => ../go-opus
)
