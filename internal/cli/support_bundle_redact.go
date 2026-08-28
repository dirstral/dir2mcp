package cli

import (
	"strconv"
	"strings"
)

// Snapshot redaction for the support bundle (#720).
//
// `.dir2mcp.yaml.snapshot` used to be copied into the archive verbatim. It
// carries `root_dir`, `state_dir` and every configured endpoint, so the default
// bundle disclosed absolute corpus paths with no --include-content consent, and
// disclosed credentials outright when an endpoint used URL userinfo
// (`https://key:secret@minio.internal`).
//
// # The two tiers
//
// The bundle already had exactly two privacy tiers, and this reuses them rather
// than inventing a third:
//
//	tier 1 — secrets      removed ALWAYS, in every mode (redactBundleSecrets)
//	tier 2 — environment  removed by default, restored by --include-content
//
// A secret is credential material: a bearer token, an API key, the userinfo of
// a URL. Tier 1 is shape-keyed and lives in redactBundleSecrets, so it applies
// to this snapshot and to server.log alike.
//
// Merely sensitive is everything that describes the operator's machine and
// deployment rather than authenticating to it: filesystem paths, hostnames and
// endpoints, bind addresses, prompts, and the glob/regex/word lists an operator
// writes by hand. A bucket name is in this class — it is not a credential, but
// `acme-legal-discovery-2026` names the client, and this file is meant to be
// pasted into a public GitHub issue. Tier 2 is exactly the class
// --include-content already gates for list-files.json's rel_path/title and
// status.json's failure samples, so path-bearing config joins it instead of
// getting a flag of its own.
//
// # Why an allow-list
//
// snapshotAllowedKeys names the keys whose values are emitted; everything else
// is redacted. The alternative — a deny-list of known-sensitive key names — was
// rejected because persistedConfig is an actively growing struct (~180 keys and
// counting). A deny-list fails OPEN: key #181 leaks until somebody remembers to
// list it, which is precisely the failure mode that produced this issue, since
// `source_s3_endpoint` was added long after the bundle's redaction was written.
// An allow-list fails CLOSED: key #181 is redacted until somebody classifies
// it. For an artifact whose purpose is public disclosure, fail-closed is the
// only defensible default.
//
// The cost of an allow-list is real — it can drop a diagnostically useful
// field — and is mitigated two ways. First, the list is generous: every tuning
// knob a maintainer triages against is on it (see the classification rule
// below), so what is lost is confined to operator-authored strings. Second,
// redaction is VISIBLE: a removed value becomes "[redacted]", so an empty value
// still means "the operator never set this" and the two cases stay
// distinguishable. Collapsing them would make the bundle actively misleading —
// a maintainer would read a redacted custom system prompt as a default one.
//
// # The classification rule
//
// A key is allowed when its value cannot carry operator environment, which is
// true in exactly two cases:
//
//   - a closed value domain — bool, number, duration, or an enum drawn from a
//     fixed vocabulary the config validator enforces (`ocr_auto`, `lenient`,
//     `memory`, `warn`, a BCP-47 language code, an AWS region);
//   - a third-party product identifier — a provider name, a model name, a
//     prompt-version tag. These name software, not the operator's deployment.
//
// Everything else — paths, URLs, hosts, bind addresses, prompts, hand-written
// pattern and word lists, and the operator-chosen names of buckets, collections
// and database objects — is tier 2.
//
// Only the bundle's COPY is rewritten. The on-disk snapshot is untouched, so
// config reload and `embed_identity` verification are unaffected.

// snapshotRedactionHeader is prepended to the redacted snapshot so the person
// reading the bundle can tell an unset key from a removed one without having to
// know this file exists.
const snapshotRedactionHeader = `# Redacted for sharing: values naming this machine or deployment (paths,
# endpoints, hosts, prompts, operator-written lists) were removed. "[redacted]"
# marks a value that WAS set and was removed; an empty value means the key was
# genuinely unset. Re-run with --include-content to keep them. Credentials are
# removed in both modes.
`

// snapshotRedactedMarker is the placeholder for a removed value. It is the same
// lower-case marker listFileRelPath already uses for content-tier redaction
// (upper-case [REDACTED] stays reserved for tier-1 secrets), and it is quoted so
// the redacted snapshot remains parseable YAML — bare [redacted] would decode
// as a one-element flow sequence.
const snapshotRedactedMarker = `"[redacted]"`

// snapshotAllowedKeys are the snapshot keys whose values survive a default
// bundle, per the classification rule documented above. Adding a key here is a
// disclosure decision: it asserts the value cannot name the operator's machine,
// deployment or corpus.
var snapshotAllowedKeys = map[string]bool{
	// Server posture. public/auth_mode carry the security posture that
	// listen_addr would otherwise be needed for; the address itself is
	// environment.
	"protocol_version": true,
	"public":           true,
	"auth_mode":        true,
	"rate_limit_rps":   true,
	"rate_limit_burst": true,

	// Timeouts and intervals.
	"session_inactivity_timeout": true,
	"session_max_lifetime":       true,
	"health_check_interval":      true,

	// RAG / retrieval tuning. The *_prompt keys are absent by design: an
	// operator-authored system prompt is free text.
	"rag_generate_answer":                   true,
	"rag_k_default":                         true,
	"rag_max_context_chars":                 true,
	"rag_oversample_factor":                 true,
	"retrieval_hybrid_enabled":              true,
	"dedup_retrieval":                       true,
	"retrieval_min_score":                   true,
	"retrieval_recency_half_life":           true,
	"context_compression_enabled":           true,
	"context_compression_target_ratio":      true,
	"retrieval_adaptive_enabled":            true,
	"retrieval_adaptive_k_min":              true,
	"retrieval_adaptive_k_max":              true,
	"retrieval_mmr_enabled":                 true,
	"retrieval_mmr_lambda":                  true,
	"retrieval_hyde_enabled":                true,
	"retrieval_hyde_superlative":            true,
	"rag_verify_faithfulness":               true,
	"retrieval_hyde_mode":                   true,
	"retrieval_contextual_enabled":          true,
	"retrieval_contextual_provider":         true,
	"retrieval_contextual_model":            true,
	"retrieval_contextual_max_tokens":       true,
	"retrieval_contextual_prompt_version":   true,
	"cross_lingual_enabled":                 true,
	"cross_lingual_target_langs":            true,
	"retrieval_hierarchical_enabled":        true,
	"retrieval_hierarchical_source_reps":    true,
	"retrieval_hierarchical_levels":         true,
	"retrieval_hierarchical_provider":       true,
	"retrieval_hierarchical_max_tokens":     true,
	"retrieval_hierarchical_prompt_version": true,
	"rerank_enabled":                        true,
	"rerank_provider":                       true,
	"rerank_model":                          true,
	"rerank_candidate_pool":                 true,

	// Chunking and ingest routing — the settings an extraction/OCR bug report
	// is actually triaged against.
	"chunking_max_tokens":     true,
	"chunking_overlap_tokens": true,
	"ingest_gitignore":        true,
	"ingest_follow_symlinks":  true,
	"ingest_max_file_mb":      true,
	"ingest_pdf_mode":         true,
	"ingest_images_mode":      true,
	"ingest_audio_mode":       true,
	"ingest_archives_mode":    true,
	"ingest_extractor":        true,
	"ingest_on_unsupported":   true,
	"index_backend":           true,
	"ingest_scan_cache":       true,
	"ingest_late_chunking":    true,
	"ingest_watch":            true,
	"ingest_watch_debounce":   true,

	// Speech-to-text and recognition selection. recognize_serve_url/command are
	// absent: a self-hosted endpoint and a command line are environment.
	"stt_provider":                 true,
	"stt_mistral_model":            true,
	"stt_elevenlabs_model":         true,
	"stt_elevenlabs_language_code": true,
	"recognize_provider":           true,

	// Quality and language.
	"quality_gates_enabled":      true,
	"language_detection_enabled": true,

	// An ElevenLabs catalog voice ID names a vendor asset, not the operator's
	// deployment, so it is a product identifier despite looking opaque. The
	// matching elevenlabs_base_url is an endpoint and is absent.
	"elevenlabs_tts_voice_id": true,

	// Media pipeline. The word/phrase lists (media_filter_words,
	// media_subtitles_glossary/drop_phrases/scrub_phrases) are absent: they are
	// hand-written and routinely derived from the corpus itself.
	"media_sidecars_disabled":                 true,
	"media_variants_group":                    true,
	"media_variants_select":                   true,
	"media_translate_enabled":                 true,
	"media_translate_target_langs":            true,
	"media_translate_engine":                  true,
	"media_subtitles_ttml_enabled":            true,
	"media_subtitles_ttml_align_tolerance_ms": true,
	"media_subtitles_smil_enabled":            true,
	"media_subtitles_segmentation":            true,
	"media_subtitles_collapse_repeats":        true,
	"media_subtitles_drop_urls":               true,
	"media_trim_leading_silence":              true,
	"media_silence_threshold_db":              true,
	"media_vad":                               true,
	"media_diarize_enabled":                   true,
	"media_audio_window_sec":                  true,
	"media_translate_whisper_window_sec":      true,
	"media_translate_window_lines":            true,
	"media_translate_context_lines":           true,
	"media_video_window_sec":                  true,
	"media_clip_max_duration_ms":              true,
	"media_clip_max_bytes":                    true,
	"media_stt_max_payload_mb":                true,
	"media_stt_request_timeout_sec":           true,
	"media_stt_language_strict":               true,
	"media_stt_on_uncovered_language":         true,
	"media_stt_tracks":                        true,
	"media_batch_two_phase":                   true,
	"media_batch_progress":                    true,

	// x402 gating. The URLs, the asset contract and the payee address are
	// absent: they identify the operator's deployment and payee.
	"x402_mode":               true,
	"x402_tools_call_enabled": true,
	"x402_price_atomic":       true,
	"x402_network":            true,
	"x402_scheme":             true,

	// Corpus source. kind and region are closed vocabularies; bucket, prefix
	// and endpoint name the operator's storage and are absent.
	"source_kind":      true,
	"source_s3_region": true,

	// Distributed embedding. distributed_embed_broker is a backend SELECTOR
	// (the credential-bearing broker URL is never persisted at all);
	// distributed_embed_sqlite_path is a path and is absent.
	"distributed_embed_enabled":      true,
	"distributed_embed_broker":       true,
	"distributed_embed_max_attempts": true,

	// Embed identity components. embed_contextual is an enum. embed_identity
	// and embed_base_url are absent: both embed the resolved endpoint, which
	// for a self-hosted provider is an internal hostname.
	"embed_contextual": true,

	// secret_sources records WHERE each credential came from (env, keychain,
	// file, session) and never a credential itself. It is the block the
	// command's own documentation points maintainers at.
	"secret_sources": true,
}

// redactConfigSnapshot rewrites the effective-config snapshot for inclusion in
// the support bundle. See the file comment for the tier model and for why the
// key set is an allow-list.
//
// The snapshot is emitted by config.marshalConfigYAML, a hand-rolled writer
// producing a flat document: `key: value`, `key:` followed by indented `- item`
// lines, and one nested `secret_sources:` block. Parsing it line-wise (rather
// than through a YAML round-trip) preserves that byte-level shape and, more
// importantly, means an unparseable or unfamiliar line is dropped rather than
// passed through.
func redactConfigSnapshot(raw []byte, includeContent bool) []byte {
	if len(raw) == 0 {
		return raw
	}
	if includeContent {
		// Consent covers the operator's own environment, never credentials:
		// tier 1 still runs.
		return []byte(redactBundleSecrets(string(raw)))
	}

	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	out := []string{strings.TrimRight(snapshotRedactionHeader, "\n")}
	for i := 0; i < len(lines); {
		key, value, ok := parseSnapshotKeyLine(lines[i])
		if !ok {
			// Blank, comment, or a shape this writer does not produce. Drop it:
			// an unrecognized line has no classification, and fail-closed is
			// the whole point of the allow-list.
			i++
			continue
		}
		block := snapshotIndentedBlock(lines[i+1:])
		out = append(out, renderSnapshotEntry(lines[i], key, value, block)...)
		i += 1 + len(block)
	}
	return []byte(strings.Join(out, "\n") + "\n")
}

// parseSnapshotKeyLine splits a top-level `key: value` line. A list header
// (`key:`) yields an empty value, which renderSnapshotEntry disambiguates using
// the following indented block.
func parseSnapshotKeyLine(line string) (key, value string, ok bool) {
	if line == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") ||
		strings.HasPrefix(line, "#") {
		return "", "", false
	}
	idx := strings.Index(line, ":")
	if idx <= 0 {
		return "", "", false
	}
	return line[:idx], strings.TrimSpace(line[idx+1:]), true
}

// snapshotIndentedBlock returns the run of indented lines belonging to the
// preceding key (list items, or the nested secret_sources entries).
func snapshotIndentedBlock(rest []string) []string {
	n := 0
	for n < len(rest) && (strings.HasPrefix(rest[n], " ") || strings.HasPrefix(rest[n], "\t")) {
		n++
	}
	return rest[:n]
}

// renderSnapshotEntry emits one snapshot key for the default (content-excluded)
// bundle: verbatim when the key is allow-listed, and a visible placeholder
// otherwise. Allow-listed values still pass through tier 1.
func renderSnapshotEntry(line, key, value string, block []string) []string {
	if snapshotAllowedKeys[key] {
		out := make([]string, 0, 1+len(block))
		out = append(out, redactBundleSecrets(line))
		for _, b := range block {
			out = append(out, redactBundleSecrets(b))
		}
		return out
	}
	if len(block) > 0 {
		// A list with entries. The COUNT is kept: it distinguishes "the
		// operator customized path_excludes" from "the operator did not",
		// which is the diagnostic the list is usually consulted for, and a
		// count discloses no entry. The count goes INSIDE the quotes so the
		// line stays a single valid YAML scalar.
		return []string{key + ": " + strconv.Quote("[redacted] ("+strconv.Itoa(len(block))+" items)")}
	}
	if value == "" || value == "[]" {
		// Unset, or an explicitly empty list. Nothing was removed, so say so
		// rather than claim a redaction that did not happen.
		return []string{line}
	}
	return []string{key + ": " + snapshotRedactedMarker}
}
