module github.com/tphakala/birdnet-go-remote-mic

go 1.27

// Default net listeners to plain TCP. Since Go 1.24 listeners use Multipath
// TCP on Linux by default, and remote-mic does not use it.
godebug multipathtcp=0

require (
	github.com/brutella/dnssd v1.2.14
	github.com/oapi-codegen/runtime v1.8.0
	github.com/quasilyte/go-ruleguard/dsl v0.3.23
	github.com/tphakala/go-audio-capture v0.8.0
	github.com/tphakala/go-audio-stream v0.6.0
	github.com/tphakala/go-opus v1.3.0
	golang.org/x/term v0.47.0
	gopkg.in/yaml.v3 v3.0.1
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/miekg/dns v1.1.73 // indirect
	github.com/tphakala/simd v1.14.0 // indirect
	github.com/vishvananda/netlink v1.3.1 // indirect
	github.com/vishvananda/netns v0.0.5 // indirect
	golang.org/x/net v0.62.0 // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
)
