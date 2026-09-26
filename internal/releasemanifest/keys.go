package releasemanifest

// trustedPublicKeys are the base64 Ed25519 public keys whose manifest
// signatures this build accepts. The release workflow signs with the private
// half of one of them (the RELEASE_MANIFEST_KEY secret) and refuses to publish
// a manifest that does not verify against this list.
//
// To rotate the key: add the new public key here and ship a release, so
// installed appliances learn it; then switch the secret to the new key; then
// drop the old public key in a later release. An appliance still running a
// release from before the new key was added cannot verify anything signed
// after the switch, so it needs one manual update; leave a long gap between
// adding the key and switching the secret.
var trustedPublicKeys = []string{
	"RWhWitn5w480CqAHvwSfVA6MBVf+9q6S11HEULsFBxg=", // key id ec47552f2043dde5, created 2026-09-26
}
