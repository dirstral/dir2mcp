package model

import "context"

type secretPolicyIdentityKey struct{}

// WithSecretPolicyIdentity returns a context that carries the identity of the
// secret policy a writer screened its documents with (SPEC §7.2). Ingest
// attaches it to every document it processes. A store uses it to decide
// whether a finalizing document write was evaluated under the policy the store
// currently records: only then may the write release a document that waits for
// a secret-policy re-evaluation.
func WithSecretPolicyIdentity(ctx context.Context, identity string) context.Context {
	return context.WithValue(ctx, secretPolicyIdentityKey{}, identity)
}

// SecretPolicyIdentityFrom returns the identity that WithSecretPolicyIdentity
// attached, or "" when the context carries none.
func SecretPolicyIdentityFrom(ctx context.Context) string {
	identity, _ := ctx.Value(secretPolicyIdentityKey{}).(string)
	return identity
}
