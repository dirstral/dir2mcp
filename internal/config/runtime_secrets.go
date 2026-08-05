package config

import "strings"

// RuntimeSecretRef describes one secret the booted daemon needs for THIS
// configuration and that has no representation in the config file.
//
// The set is defined by a single, checkable rule (#722): a value belongs here
// when it is resolvable ONLY from the runtime credential sources of SPEC
// §16.1.1 (environment → keychain → .env.local/.env) because the config-file
// key for it is deliberately absent or ignored. `source.s3.region` has a file
// home and is therefore NOT a runtime secret; `AWS_SECRET_ACCESS_KEY`,
// `QDRANT_API_KEY`, `index.pgvector.dsn`, `distributed_embed.broker_url`, and
// `x402_facilitator_token` have none and are.
//
// Provider-profile credentials are NOT included: they are already enumerated by
// ProviderEnvVarRefs, which understands `${VAR}` references in user-defined
// profiles. RuntimeSecretRefs covers everything else.
type RuntimeSecretRef struct {
	// Name is the environment variable the daemon reads the value from. It is
	// also the keychain account name and the .env.local key, so one name
	// identifies the value across every source.
	Name string
	// Feature is the config setting that pulls this secret in, for
	// operator-facing messages ("source.kind=s3"). Never a value.
	Feature string
	// Required reports whether the daemon fails to boot without the value.
	// Optional secrets (an unsecured Qdrant, an absent session token) are still
	// worth persisting when present, but their absence is not an error.
	Required bool
	// Resolved reports whether the effective config already carries a value.
	// On an env-aware load this is true when ANY source supplied it, so callers
	// must compare against the current process environment to tell "only in
	// this shell" from "already persisted".
	Resolved bool
}

// RuntimeSecretRefs returns the non-provider secrets the effective config
// depends on, in a deterministic order. Only secrets for ACTIVE features are
// returned: a local corpus contributes no AWS refs, a memory index contributes
// no Qdrant/pgvector refs.
//
// `dir2mcp service install` uses this to persist runtime-only credentials the
// supervised daemon could otherwise never see (launchd/systemd start from a
// clean environment) and to name the ones that have no persistent source.
func (cfg Config) RuntimeSecretRefs() []RuntimeSecretRef {
	var refs []RuntimeSecretRef
	refs = append(refs, cfg.sourceSecretRefs()...)
	refs = append(refs, cfg.indexSecretRefs()...)
	refs = append(refs, cfg.distributedEmbedSecretRefs()...)
	refs = append(refs, cfg.x402SecretRefs()...)
	return refs
}

// RuntimeSecretNames returns just the environment-variable names from
// RuntimeSecretRefs, for callers that only need the key set.
func (cfg Config) RuntimeSecretNames() []string {
	refs := cfg.RuntimeSecretRefs()
	names := make([]string, 0, len(refs))
	for _, ref := range refs {
		names = append(names, ref.Name)
	}
	return names
}

// sourceSecretRefs returns the S3 corpus-source credentials. They are populated
// by applySourceEnvOverrides on env-aware loads only and are never written to
// the config file or the effective-config snapshot, so a supervised daemon can
// see them only through the environment, the keychain, or .env.local.
//
// The access key and secret are REQUIRED: validateSourceRuntimeSecrets rejects
// an s3 source without both, so a service installed without them cannot boot.
// The session token is optional (only STS/temporary credentials carry one).
func (cfg Config) sourceSecretRefs() []RuntimeSecretRef {
	if strings.ToLower(strings.TrimSpace(cfg.Source.Kind)) != "s3" {
		return nil
	}
	const feature = "source.kind=s3"
	return []RuntimeSecretRef{
		{Name: "AWS_ACCESS_KEY_ID", Feature: feature, Required: true, Resolved: isSet(cfg.Source.S3AccessKeyID)},
		{Name: "AWS_SECRET_ACCESS_KEY", Feature: feature, Required: true, Resolved: isSet(cfg.Source.S3SecretAccessKey)},
		{Name: "AWS_SESSION_TOKEN", Feature: feature, Required: false, Resolved: isSet(cfg.Source.S3SessionToken)},
	}
}

// indexSecretRefs returns the Tier C vector-store credential for the selected
// index backend. The pgvector DSN is required (loadPgvectorIndex fails without
// it, and `index.pgvector.dsn` has no config-file setter); the Qdrant API key is
// optional because a local/unsecured instance needs none.
func (cfg Config) indexSecretRefs() []RuntimeSecretRef {
	switch strings.ToLower(strings.TrimSpace(cfg.IndexBackend)) {
	case "qdrant":
		return []RuntimeSecretRef{{
			Name: "QDRANT_API_KEY", Feature: "index.backend=qdrant",
			Required: false, Resolved: isSet(cfg.Qdrant.APIKey),
		}}
	case "pgvector":
		return []RuntimeSecretRef{{
			Name: "DIR2MCP_INDEX_PGVECTOR_DSN", Feature: "index.backend=pgvector",
			Required: true, Resolved: isSet(cfg.IndexPgvectorDSN),
		}}
	default:
		return nil
	}
}

// distributedEmbedSecretRefs returns the external broker connection URL, which
// may embed credentials and is therefore runtime-only (SPEC §16.1.1).
//
// It is reported as OPTIONAL on purpose: this build ships only the in-process
// "memory" and "sqlite" brokers (validateDistributedEmbed rejects anything
// else), neither of which reads BrokerURL, so requiring it would raise a false
// alarm on every valid distributed deployment. It is still captured when set so
// an external-broker deployment does not lose it at reboot.
func (cfg Config) distributedEmbedSecretRefs() []RuntimeSecretRef {
	if !cfg.DistributedEmbed.Enabled {
		return nil
	}
	return []RuntimeSecretRef{{
		Name: "DIR2MCP_DISTRIBUTED_EMBED_BROKER_URL", Feature: "distributed_embed.enabled",
		Required: false, Resolved: isSet(cfg.DistributedEmbed.BrokerURL),
	}}
}

// x402SecretRefs returns the facilitator bearer token. The config loader
// deliberately ignores a file value for it, so it is environment/keychain-only.
// It is optional: a facilitator may accept unauthenticated settlement calls.
func (cfg Config) x402SecretRefs() []RuntimeSecretRef {
	mode := strings.ToLower(strings.TrimSpace(cfg.X402.Mode))
	if mode == "" || mode == "off" || !isSet(cfg.X402.FacilitatorURL) {
		return nil
	}
	return []RuntimeSecretRef{{
		Name: "DIR2MCP_X402_FACILITATOR_TOKEN", Feature: "x402.mode=" + mode,
		Required: false, Resolved: isSet(cfg.X402.FacilitatorToken),
	}}
}

// isSet reports whether a resolved config value carries content.
func isSet(v string) bool { return strings.TrimSpace(v) != "" }
