//go:build linux

package service

import (
	"bytes"
	"path/filepath"
	"text/template"

	"github.com/tphakala/birdnet-go-remote-mic/internal/update"
)

// unitTemplate is the systemd unit rendered for an install. Choices worth
// noting: Restart=always (not on-failure) is required because a UI-initiated or
// self-update restart exits the process cleanly (code 0) and relies on the
// supervisor to bring it back, which on-failure would not do; ordering after
// remote-fs.target makes a certificate or config on a network mount less likely
// to be missing at start (local mounts already precede every default-dependency
// service, and a management API that still cannot read its certificate retries
// in the background); ExecStartPre is
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
After=network-online.target sound.target remote-fs.target
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

// updatePathTemplate starts the root updater when the appliance writes an
// update request into the staging directory, and when an install journal is
// found beside the binary (an update cut off by a power loss or a kill, which
// the updater rolls back on its next start, even after a reboot). Nothing
// listens and nothing runs until then; the updater claims the request by
// renaming it and removes the journal whatever the outcome, so the unit does
// not start it again for the same one.
const updatePathTemplate = `[Unit]
Description=Watch for a staged remote-mic update
Documentation=https://github.com/tphakala/birdnet-go-remote-mic

[Path]
PathExists={{.RequestPath}}
PathExists={{.BinPath}}.pending
Unit=` + UpdateServiceUnit + `

[Install]
WantedBy=multi-user.target
`

// updateServiceTemplate is the root updater: it verifies the staged release
// itself (against the keys in the installed binary), swaps the binary,
// restarts the appliance and rolls back when the new version does not come
// up. It needs no network, and may write only the binary's directory and the
// state directory; restarting the appliance goes through systemd's own
// socket, which PrivateNetwork does not affect. StartLimitBurst keeps a
// request or journal the updater somehow cannot clear from restarting it in a
// loop.
//
// Self-updated appliances keep the units their install wrote, so every later
// release is started by this ExecStart line: its subcommand and flags are a
// contract between versions (see AGENTS.md, Releases).
const updateServiceTemplate = `[Unit]
Description=Install a staged remote-mic update
Documentation=https://github.com/tphakala/birdnet-go-remote-mic
StartLimitIntervalSec=1h
StartLimitBurst=5

[Service]
Type=oneshot
ExecStart={{.BinPath}} service apply-update --bin-path={{.BinPath}} --state-dir={{.StateDir}}
TimeoutStartSec=10min
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateNetwork=true
ReadWritePaths={{.BinDir}} {{.StateDir}}
`

// The templates are parsed once at package init; their text is a compile-time
// constant, so a parse failure is a programming error and panicking is correct.
var (
	unitTmpl          = template.Must(template.New("unit").Parse(unitTemplate))
	updatePathTmpl    = template.Must(template.New("update-path").Parse(updatePathTemplate))
	updateServiceTmpl = template.Must(template.New("update-service").Parse(updateServiceTemplate))
)

// unitData is the flattened view the template renders from, with ConfigDir
// derived from ConfigPath so the template stays free of path logic.
type unitData struct {
	User, Group, BinPath, BinDir, ConfigPath, ConfigDir, StateDir, RequestPath string
}

// Render fills in the systemd unit for s. It applies defaults and validates
// first, so a caller that passes a partial or bad spec gets a clear error
// rather than a malformed unit.
func Render(s ServiceSpec) ([]byte, error) {
	return render(unitTmpl, s)
}

// RenderUpdater fills in the root updater's path and service units for s.
func RenderUpdater(s ServiceSpec) (pathUnit, serviceUnit []byte, err error) {
	if pathUnit, err = render(updatePathTmpl, s); err != nil {
		return nil, nil, err
	}
	if serviceUnit, err = render(updateServiceTmpl, s); err != nil {
		return nil, nil, err
	}
	return pathUnit, serviceUnit, nil
}

func render(tmpl *template.Template, s ServiceSpec) ([]byte, error) {
	s = s.withDefaults()
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, unitData{
		User:        s.User,
		Group:       s.User, // the service user's primary group takes its name
		BinPath:     s.BinPath,
		BinDir:      filepath.Dir(s.BinPath),
		ConfigPath:  s.ConfigPath,
		ConfigDir:   s.ConfigDir(),
		StateDir:    s.StateDir,
		RequestPath: filepath.Join(s.UpdateDir(), update.RequestFile),
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
