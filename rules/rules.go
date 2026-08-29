//go:build ruleguard

// Package rules holds gocritic ruleguard matchers enforced via
// golangci-lint. The build tag keeps them out of normal builds.
package rules

import "github.com/quasilyte/go-ruleguard/dsl"

// banFmtErrorfWithoutArgs nudges toward errors.New for constant messages.
func banFmtErrorfWithoutArgs(m dsl.Matcher) {
	m.Match(`fmt.Errorf($msg)`).
		Where(m["msg"].Const).
		Suggest(`errors.New($msg)`).
		Report(`use errors.New for constant messages`)
}
