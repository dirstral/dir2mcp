package cli

import (
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/dirstral/dir2mcp/internal/config"
)

// providerKeySpec describes one credential the setup wizard can collect. The
// EnvVar is the dotenv key written to .env.local; secrets are never persisted
// to the .dir2mcp.yaml snapshot (SPEC §16: api keys are env-sourced).
type providerKeySpec struct {
	EnvVar      string
	Title       string
	Description string
	Optional    bool // shown only when the user opts into optional providers
}

// wizardProviderKeys is the ordered set of credentials the wizard offers. The
// required Mistral key and the rerank-enabling Cohere key are always shown; the
// rest are revealed only when the user asks to configure optional providers, so
// the common path stays a two-field form.
var wizardProviderKeys = []providerKeySpec{
	{EnvVar: "MISTRAL_API_KEY", Title: "Mistral API key", Description: "Required — embeddings, PDF/OCR extraction, and answers."},
	{EnvVar: "COHERE_API_KEY", Title: "Cohere API key", Description: "Optional — enables reranking (sharper citations)."},
	{EnvVar: "OPENAI_API_KEY", Title: "OpenAI API key", Description: "Optional — alternate chat/embedding provider.", Optional: true},
	{EnvVar: "ANTHROPIC_API_KEY", Title: "Anthropic API key", Description: "Optional — alternate chat provider.", Optional: true},
	{EnvVar: "GEMINI_API_KEY", Title: "Gemini API key", Description: "Optional — chat, embeddings, and audio.", Optional: true},
	{EnvVar: "ELEVENLABS_API_KEY", Title: "ElevenLabs API key", Description: "Optional — speech-to-text / text-to-speech.", Optional: true},
}

// corpusProfile is a named retrieval preset the wizard can apply to the config.
// "general" intentionally changes nothing (keeps Config defaults / any existing
// values); the others tune retrieval for a document class.
type corpusProfile string

const (
	corpusProfileGeneral corpusProfile = "general"
	corpusProfileLegal   corpusProfile = "legal"
	corpusProfileCode    corpusProfile = "code"
)

const legalSystemPrompt = `You answer questions strictly from the provided legal documents: statutes,
amendment acts, regulations, and codes of practice. Cite the specific act,
section, and page for every statement. When provisions conflict, prefer the
most recent and say which one applies. If the documents do not cover the
question, say so plainly. Do not give legal advice or speculate beyond the
cited text.`

const codeSystemPrompt = `You answer questions strictly from the provided source code and project
documentation. Cite file paths and line ranges, and quote the relevant code.
If the indexed code does not cover the question, say so plainly rather than
guessing.`

// applyCorpusProfile mutates cfg's retrieval settings to match the chosen
// profile. It is a pure function over cfg (no IO) so the mapping is unit
// testable without driving the TUI. Unknown/general profiles leave cfg
// untouched, preserving Config defaults and any pre-existing user values.
func applyCorpusProfile(cfg *config.Config, profile corpusProfile) {
	switch profile {
	case corpusProfileLegal:
		cfg.RAGKDefault = 12
		cfg.RAGMaxContextChars = 40000
		cfg.RAGSystemPrompt = legalSystemPrompt
	case corpusProfileCode:
		cfg.RAGKDefault = 8
		cfg.RAGSystemPrompt = codeSystemPrompt
	default:
		// general / unknown: no overrides.
	}
}

// wizardResult holds the user's answers from the setup form: the collected
// credentials (keyed by env var, empty entries dropped) and the chosen corpus
// profile.
type wizardResult struct {
	Keys    map[string]string
	Profile corpusProfile
}

// buildSetupForm constructs the huh form, binding each field to the provided
// pointers. Optional provider inputs live in a group hidden until the user
// confirms they want to configure more providers. Kept separate from execution
// so the field/grouping layout can be reasoned about (and the form built) apart
// from the interactive Run.
func buildSetupForm(
	keyValues map[string]*string,
	configureMore *bool,
	profile *string,
) *huh.Form {
	required := []huh.Field{}
	optional := []huh.Field{}
	for _, spec := range wizardProviderKeys {
		input := huh.NewInput().
			Title(spec.Title).
			Description(spec.Description).
			EchoMode(huh.EchoModePassword).
			Value(keyValues[spec.EnvVar])
		if spec.Optional {
			optional = append(optional, input)
		} else {
			required = append(required, input)
		}
	}

	providerGroup := huh.NewGroup(append(
		required,
		huh.NewConfirm().
			Title("Configure optional chat / audio providers?").
			Description("OpenAI, Anthropic, Gemini, ElevenLabs.").
			Value(configureMore),
	)...)

	optionalGroup := huh.NewGroup(optional...).
		WithHideFunc(func() bool { return !*configureMore })

	profileGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Corpus profile").
			Description("Tunes retrieval for this kind of content.").
			Options(
				huh.NewOption("General documents", string(corpusProfileGeneral)),
				huh.NewOption("Legal / citations (strict grounding)", string(corpusProfileLegal)),
				huh.NewOption("Source code", string(corpusProfileCode)),
			).
			Value(profile),
	)

	return huh.NewForm(providerGroup, optionalGroup, profileGroup)
}

// runSetupWizard runs the interactive huh setup form and returns the user's
// answers. Credentials are returned (not yet written) so the caller controls
// persistence (.env.local) and config patching.
func runSetupWizard() (wizardResult, error) {
	keyValues := make(map[string]*string, len(wizardProviderKeys))
	for _, spec := range wizardProviderKeys {
		v := new(string)
		keyValues[spec.EnvVar] = v
	}
	var configureMore bool
	profile := string(corpusProfileGeneral)

	form := buildSetupForm(keyValues, &configureMore, &profile)
	if err := form.Run(); err != nil {
		return wizardResult{}, err
	}

	keys := make(map[string]string)
	for env, ptr := range keyValues {
		if v := strings.TrimSpace(*ptr); v != "" {
			keys[env] = v
		}
	}
	return wizardResult{Keys: keys, Profile: corpusProfile(profile)}, nil
}
