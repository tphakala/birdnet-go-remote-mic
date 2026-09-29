//go:build !unix || aix || solaris

package update

import (
	"context"
	"time"
)

// LockBin has nothing to lock with here: the appliance and its updater run
// on Linux, and this keeps the package building elsewhere.
func LockBin(context.Context, string, time.Duration, func()) (release func(), err error) {
	return func() {}, nil
}
