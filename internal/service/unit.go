//go:build linux

package service

import (
	"bytes"
	"text/template"
)

// unitTemplate is the systemd unit rendered for an install. Choices worth
// noting: Restart=always (not on-failure) is required because a UI-initiated or
// self-update restart exits the process cleanly (code 0) and relies on the
// supervisor to bring it back, which on-failure would not do; ordering after
// local-fs.target and remote-fs.target makes a certificate or config on a
// late-mounted volume less likely to be missing at start (a management API that
// still cannot read its certificate retries in the background); ExecStartPre is
// "-"prefixed so a broken hand-edited config logs but does not block the web UI
// from coming up to fix it; SupplementaryGroups=audio grants /dev/snd without
// running as root; ProtectSystem=strict makes the filesystem read-only except
// the config dir and state dir listed in ReadWritePaths. Device and
// address-family restriction is deliberately left out: it can silently break
// ALSA capture or mDNS interface enumeration and needs validation on real
// hardware first.
const unitTemplate = `[Unit]
Description=BirdNET-Go remote microphone appliance
Documentation=https://github.com/tphakala/birdnet-go-remote-mic
After=network-online.target sound.target local-fs.target remote-fs.target
Wants=network-online.target

[Service]
Type=simple
User={{.User}}
Group={{.Group}}
SupplementaryGroups=audio
Environment=REMOTEMIC_CONFIG={{.ConfigPath}}
ExecStartPre=-{{.BinPath}} serve --check --cert-dir={{.StateDir}}
ExecStart={{.BinPath}} serve --cert-dir={{.StateDir}}
# Restart on any exit, including the clean exit a UI-initiated or self-update
# restart makes; on-failure would leave the appliance stopped after one.
Restart=always
RestartSec=5
WorkingDirectory={{.StateDir}}
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths={{.ConfigDir}} {{.StateDir}}

[Install]
WantedBy=multi-user.target
`

// unitTmpl is parsed once at package init; the template text is a compile-time
// constant, so a parse failure is a programming error and panicking is correct.
var unitTmpl = template.Must(template.New("unit").Parse(unitTemplate))

// unitData is the flattened view the template renders from, with ConfigDir
// derived from ConfigPath so the template stays free of path logic.
type unitData struct {
	User, Group, BinPath, ConfigPath, ConfigDir, StateDir string
}

// Render fills in the systemd unit for s. It applies defaults and validates
// first, so a caller that passes a partial or bad spec gets a clear error
// rather than a malformed unit.
func Render(s ServiceSpec) ([]byte, error) {
	s = s.withDefaults()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := unitTmpl.Execute(&buf, unitData{
		User:       s.User,
		Group:      s.User, // the service user's primary group takes its name
		BinPath:    s.BinPath,
		ConfigPath: s.ConfigPath,
		ConfigDir:  s.ConfigDir(),
		StateDir:   s.StateDir,
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
