package rtspserver

// authorizedUnder reports whether a connection is still authorized while the
// guard is at guardGen. A connection records authGen as the generation it
// authenticated under; authentication holds only for that exact generation.
// Enabling or rotating the token advances the generation, so a connection
// authenticated under the old token, or one that never authenticated at all
// (authed=false), is no longer authorized.
func authorizedUnder(authed bool, authGen, guardGen uint64) bool {
	return authed && authGen == guardGen
}

// shouldEvict reports whether the media writer must drop a connection now: an
// enabled guard that this connection is not currently authorized under. Gating
// on authorizedUnder rather than a bare authGen != guardGen compare keeps it
// correct even for an enabled guard sitting at generation 0 (a hypothetical
// future Guard that seeds its state without routing through Set): an
// open-access connection has authed=false, so it is evicted the moment a token
// is enabled regardless of the generation value, instead of slipping through a
// 0 != 0 compare that would pass.
func shouldEvict(enabled bool, guardGen uint64, authed bool, authGen uint64) bool {
	return enabled && !authorizedUnder(authed, authGen, guardGen)
}
