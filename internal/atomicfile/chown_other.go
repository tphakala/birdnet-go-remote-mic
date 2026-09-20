//go:build !linux

package atomicfile

import "os"

// preserveOwner is a no-op off Linux: the appliance is Linux-only, and the
// ownership hazard it guards against (a sudo-run writer re-homing a
// service-user file to root) does not arise on the platforms that only build
// this package for tooling. Keeping a stub here preserves atomicfile's
// portability.
func preserveOwner(_ *os.File, _ string) {}
