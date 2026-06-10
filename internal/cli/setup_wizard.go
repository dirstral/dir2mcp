package cli

import (
	"errors"
	"os"
	"path/filepath"
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
	{EnvVar: "COHERE_API_KEY", Title: "Cohere API key", Description: "Recommended — enables reranking (sharper citations)."},
	{EnvVar: "OPENAI_API_KEY", Title: "OpenAI API key", Description: "Optional — alternate chat/embedding provider.", Optional: true},
	{EnvVar: "ANTHROPIC_API_KEY", Title: "Anthropic API key", Description: "Optional — alternate chat provider.", Optional: true},
	{EnvVar: "GEMINI_API_KEY", Title: "Gemini API key", Description: "Optional — chat, embeddings, and audio.", Optional: true},
	{EnvVar: "ELEVENLABS_API_KEY", Title: "ElevenLabs API key", Description: "Optional — speech-to-text / text-to-speech.", Optional: true},
}

const mistralEnvVar = "MISTRAL_API_KEY"

// corpusProfile is a named retrieval preset the wizard can apply to the config.
type corpusProfile string

const (
	// corpusProfileKeep leaves retrieval settings untouched (offered only when a
	// config already exists, so re-running the wizard need not re-tune).
	corpusProfileKeep    corpusProfile = "keep"
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
// testable without driving the TUI.
//
// Each preset is self-contained: the managed retrieval fields are first reset
// to Config defaults, then profile-specific overrides are layered on. This way
// re-running the wizard and switching profiles (e.g. legal → code) can never
// inherit a stale value tuned for the previous profile. "general" is therefore
// the baseline (Config defaults). "keep" is the sole exception: it leaves cfg
// untouched so an existing tuned config survives a re-run.
func applyCorpusProfile(cfg *config.Config, profile corpusProfile) {
	if profile == corpusProfileKeep {
		return
	}

	def := config.Default()
	cfg.RAGKDefault = def.RAGKDefault
	cfg.RAGMaxContextChars = def.RAGMaxContextChars
	cfg.RAGSystemPrompt = def.RAGSystemPrompt

	switch profile {
	case corpusProfileLegal:
		cfg.RAGKDefault = 12
		cfg.RAGMaxContextChars = 40000
		cfg.RAGSystemPrompt = legalSystemPrompt
	case corpusProfileCode:
		cfg.RAGKDefault = 8
		cfg.RAGSystemPrompt = codeSystemPrompt
	default:
		// general: Config defaults (already applied above).
	}
}

// wizardResult holds the user's answers from the setup form: the collected
// credentials (keyed by env var, empty entries dropped) and the chosen corpus
// profile.
type wizardResult struct {
	Keys    map[string]string
	Profile corpusProfile
}

// wizardInput parameterizes the setup form with what is already known about the
// environment so the form can adapt: which credentials are already set (so the
// field can say "leave blank to keep" and the required check can pass), and
// whether a config already exists (so a "keep current settings" profile option
// is offered and pre-selected).
type wizardInput struct {
	ExistingKeys  map[string]bool
	ConfigExisted bool
}

// keyDescription augments a provider field's static description with a note when
// the credential is already present in the environment / .env.local.
func keyDescription(spec providerKeySpec, existing map[string]bool) string {
	if existing[spec.EnvVar] {
		return spec.Description + " (already set — leave blank to keep)"
	}
	return spec.Description
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
	save *bool,
	in wizardInput,
) *huh.Form {
	required := []huh.Field{}
	optional := []huh.Field{}
	for _, spec := range wizardProviderKeys {
		input := huh.NewInput().
			Title(spec.Title).
			Description(keyDescription(spec, in.ExistingKeys)).
			EchoMode(huh.EchoModePassword).
			Value(keyValues[spec.EnvVar])
		// Mistral is required: reject an empty value unless it is already set
		// (in which case a blank field means "keep the existing key").
		if spec.EnvVar == mistralEnvVar && !in.ExistingKeys[mistralEnvVar] {
			input = input.Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("required — paste your Mistral API key (or pre-set MISTRAL_API_KEY)")
				}
				return nil
			})
		}
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

	options := []huh.Option[string]{}
	if in.ConfigExisted {
		options = append(options, huh.NewOption("Keep current settings (don't change retrieval)", string(corpusProfileKeep)))
	}
	options = append(options,
		huh.NewOption("General documents", string(corpusProfileGeneral)),
		huh.NewOption("Legal / citations (strict grounding)", string(corpusProfileLegal)),
		huh.NewOption("Source code", string(corpusProfileCode)),
	)
	profileGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Corpus profile").
			Description("Tunes retrieval for this kind of content.").
			Options(options...).
			Value(profile),
	)

	confirmGroup := huh.NewGroup(
		huh.NewConfirm().
			Title("Save these settings?").
			Affirmative("Save").
			Negative("Cancel").
			Value(save),
	)

	return huh.NewForm(providerGroup, optionalGroup, profileGroup, confirmGroup)
}

// runSetupWizard runs the interactive huh setup form and returns the user's
// answers. Credentials are returned (not yet written) so the caller controls
// persistence (.env.local) and config patching. Declining the final "Save"
// confirm surfaces as huh.ErrUserAborted so callers treat it like Ctrl-C.
func runSetupWizard(in wizardInput) (wizardResult, error) {
	keyValues := make(map[string]*string, len(wizardProviderKeys))
	for _, spec := range wizardProviderKeys {
		keyValues[spec.EnvVar] = new(string)
	}
	var configureMore bool
	save := true
	profile := string(corpusProfileGeneral)
	if in.ConfigExisted {
		profile = string(corpusProfileKeep)
	}

	form := buildSetupForm(keyValues, &configureMore, &profile, &save, in)
	if err := form.Run(); err != nil {
		return wizardResult{}, err
	}
	if !save {
		return wizardResult{}, huh.ErrUserAborted
	}

	keys := make(map[string]string)
	for env, ptr := range keyValues {
		if v := strings.TrimSpace(*ptr); v != "" {
			keys[env] = v
		}
	}
	return wizardResult{Keys: keys, Profile: corpusProfile(profile)}, nil
}

// detectExistingKeys reports which wizard credentials already resolve, either
// from the process environment or from a non-empty assignment in .env.local at
// envPath. Used to relax the required check and annotate fields.
func detectExistingKeys(envPath string) map[string]bool {
	out := make(map[string]bool, len(wizardProviderKeys))
	var dotenv string
	if b, err := os.ReadFile(envPath); err == nil {
		dotenv = string(b)
	}
	for _, spec := range wizardProviderKeys {
		if strings.TrimSpace(os.Getenv(spec.EnvVar)) != "" || dotenvHasKey(dotenv, spec.EnvVar) {
			out[spec.EnvVar] = true
		}
	}
	return out
}

// dotenvHasKey reports whether content has a non-empty assignment for key,
// honoring an optional leading "export " and treating quoted-empty values
// (KEY="" / KEY=”) as unset.
func dotenvHasKey(content, key string) bool {
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		if strings.HasPrefix(line, key+"=") {
			val := strings.TrimSpace(strings.TrimPrefix(line, key+"="))
			if unquoteDotenvValue(val) != "" {
				return true
			}
		}
	}
	return false
}

// unquoteDotenvValue strips a single pair of matching surrounding quotes from a
// dotenv value so KEY="" / KEY=” read as empty.
func unquoteDotenvValue(val string) string {
	if len(val) >= 2 {
		if (val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'') {
			return val[1 : len(val)-1]
		}
	}
	return val
}

// ensureGitignoreEntries appends any missing entries to dir/.gitignore (creating
// it if needed), so credentials and local state are not accidentally committed.
// Existing entries and file content are preserved.
func ensureGitignoreEntries(dir string, entries ...string) error {
	path := filepath.Join(dir, ".gitignore")
	var existing string
	if b, err := os.ReadFile(path); err == nil {
		existing = string(b)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	have := make(map[string]bool)
	for _, line := range strings.Split(existing, "\n") {
		have[strings.TrimSpace(line)] = true
	}
	var toAdd []string
	for _, e := range entries {
		if !have[e] {
			toAdd = append(toAdd, e)
		}
	}
	if len(toAdd) == 0 {
		return nil
	}

	var b strings.Builder
	b.WriteString(existing)
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		b.WriteString("\n")
	}
	for _, e := range toAdd {
		b.WriteString(e + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}
