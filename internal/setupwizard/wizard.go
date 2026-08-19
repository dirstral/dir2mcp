// Package setupwizard holds the provider-credential and corpus-profile logic
// behind `dir2mcp config init` and the `dir2mcp up` first-run flow (SPEC §§
// config init / charmbracelet/huh). It is a standalone package so the pure
// mapping and IO helpers are unit-testable from tests/ (the repo forbids new
// *_test.go files under internal/); the CLI package wires these into command
// output, provider verification, and persistence.
package setupwizard

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/dirstral/dir2mcp/internal/config"
)

// dir2mcp brand palette (matches internal/cli/style.go): orange #F2911A is the
// primary accent, yellow the secondary highlight, on a black button base.
var (
	brandOrange = lipgloss.Color("208")
	brandYellow = lipgloss.Color("220")
	brandBlack  = lipgloss.Color("0")
	brandDim    = lipgloss.Color("245")
	brandRed    = lipgloss.Color("203")
)

// brandTheme returns a huh theme styled with the dir2mcp brand colors (orange
// accents, yellow selection highlights, black-on-orange action buttons).
func brandTheme() *huh.Theme {
	t := huh.ThemeBase()

	f := &t.Focused
	f.Base = f.Base.BorderForeground(brandOrange)
	f.Card = f.Base
	f.Title = f.Title.Foreground(brandOrange).Bold(true)
	f.NoteTitle = f.NoteTitle.Foreground(brandOrange).Bold(true)
	f.Description = f.Description.Foreground(brandDim)
	f.SelectSelector = f.SelectSelector.Foreground(brandYellow)
	f.MultiSelectSelector = f.MultiSelectSelector.Foreground(brandYellow)
	f.SelectedOption = f.SelectedOption.Foreground(brandYellow)
	f.SelectedPrefix = f.SelectedPrefix.Foreground(brandYellow)
	f.NextIndicator = f.NextIndicator.Foreground(brandYellow)
	f.PrevIndicator = f.PrevIndicator.Foreground(brandYellow)
	f.ErrorIndicator = f.ErrorIndicator.Foreground(brandRed)
	f.ErrorMessage = f.ErrorMessage.Foreground(brandRed)
	f.FocusedButton = f.FocusedButton.Foreground(brandBlack).Background(brandOrange).Bold(true)
	f.BlurredButton = f.BlurredButton.Foreground(brandDim).Background(brandBlack)
	f.TextInput.Cursor = f.TextInput.Cursor.Foreground(brandYellow)
	f.TextInput.Prompt = f.TextInput.Prompt.Foreground(brandOrange)
	f.TextInput.Placeholder = f.TextInput.Placeholder.Foreground(brandDim)

	// Blurred (inactive) groups mirror focused styling but hide the border and
	// dim the title so the active group stands out.
	t.Blurred = t.Focused
	t.Blurred.Base = t.Blurred.Base.BorderStyle(lipgloss.HiddenBorder())
	t.Blurred.Card = t.Blurred.Base
	t.Blurred.Title = t.Blurred.Title.Foreground(brandDim).Bold(false)

	return t
}

// ProviderKeySpec describes one credential the setup wizard can collect. The
// EnvVar is the dotenv key written to .env.local; secrets are never persisted
// to the .dir2mcp.yaml snapshot (SPEC §16: api keys are env-sourced).
type ProviderKeySpec struct {
	EnvVar      string
	Title       string
	Description string
	Optional    bool // shown only when the user opts into optional providers
}

// MistralEnvVar is the required embedding/OCR/answer credential.
const MistralEnvVar = "MISTRAL_API_KEY"

// ProviderKeys is the ordered set of credentials the wizard offers. The required
// Mistral key and the rerank-enabling Cohere key are always shown; the rest are
// revealed only when the user asks to configure optional providers, so the
// common path stays a two-field form.
var ProviderKeys = []ProviderKeySpec{
	{EnvVar: MistralEnvVar, Title: "Mistral API key", Description: "Required — embeddings, PDF/OCR extraction, and answers."},
	{EnvVar: "COHERE_API_KEY", Title: "Cohere API key", Description: "Recommended — enables reranking (sharper citations)."},
	{EnvVar: "OPENAI_API_KEY", Title: "OpenAI API key", Description: "Optional — alternate chat/embedding provider.", Optional: true},
	{EnvVar: "ANTHROPIC_API_KEY", Title: "Anthropic API key", Description: "Optional — alternate chat provider.", Optional: true},
	{EnvVar: "GEMINI_API_KEY", Title: "Gemini API key", Description: "Optional — chat, embeddings, and audio.", Optional: true},
	{EnvVar: "ELEVENLABS_API_KEY", Title: "ElevenLabs API key", Description: "Optional — speech-to-text / text-to-speech.", Optional: true},
}

// Profile is a named retrieval preset the wizard can apply to the config.
type Profile string

const (
	// ProfileKeep leaves retrieval settings untouched (offered only when a
	// config already exists, so re-running the wizard need not re-tune).
	ProfileKeep    Profile = "keep"
	ProfileGeneral Profile = "general"
	ProfileLegal   Profile = "legal"
	ProfileCode    Profile = "code"
)

// legalSystemPrompt and codeSystemPrompt carry domain instructions only. The
// server appends the prompt-injection guard to whatever system prompt is in
// force (internal/retrieval, issue #885), so a preset cannot drop it. Before
// #885 these presets replaced the whole prompt and did drop it: the legal
// preset reads statutes and contracts, which an adversary may supply.
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

// ApplyCorpusProfile mutates cfg's retrieval settings to match the chosen
// profile. It is a pure function over cfg (no IO) so the mapping is unit
// testable without driving the TUI.
//
// Each preset is self-contained: the managed retrieval fields are first reset
// to Config defaults, then profile-specific overrides are layered on. This way
// re-running the wizard and switching profiles (e.g. legal → code) can never
// inherit a stale value tuned for the previous profile. "general" is therefore
// the baseline (Config defaults). "keep" is the sole exception: it leaves cfg
// untouched so an existing tuned config survives a re-run.
func ApplyCorpusProfile(cfg *config.Config, profile Profile) {
	if profile == ProfileKeep {
		return
	}

	def := config.Default()
	cfg.RAGKDefault = def.RAGKDefault
	cfg.RAGMaxContextChars = def.RAGMaxContextChars
	cfg.RAGSystemPrompt = def.RAGSystemPrompt

	switch profile {
	case ProfileLegal:
		cfg.RAGKDefault = 12
		cfg.RAGMaxContextChars = 40000
		cfg.RAGSystemPrompt = legalSystemPrompt
	case ProfileCode:
		// Code uses a tighter top-k and the standard context window; all three
		// managed fields are set explicitly so the preset is self-contained.
		cfg.RAGKDefault = 8
		cfg.RAGMaxContextChars = def.RAGMaxContextChars
		cfg.RAGSystemPrompt = codeSystemPrompt
	default:
		// general: Config defaults (already applied above).
	}
}

// SecretDest is where the wizard persists the collected credentials.
type SecretDest string

const (
	// DestFile writes credentials to .env.local (plaintext, 0600). Works with
	// the background daemon.
	DestFile SecretDest = "file"
	// DestKeychain stores credentials in the OS keychain (encrypted at rest).
	DestKeychain SecretDest = "keychain"
)

// Result holds the user's answers from the setup form: the collected
// credentials (keyed by env var, empty entries dropped), the chosen corpus
// profile, and where to persist the credentials.
type Result struct {
	Keys        map[string]string
	Profile     Profile
	Destination SecretDest
}

// Input parameterizes the setup form with what is already known about the
// environment so the form can adapt: which credentials are already set (so the
// field can say "leave blank to keep" and the required check can pass), and
// whether a config already exists (so a "keep current settings" profile option
// is offered and pre-selected).
type Input struct {
	ExistingKeys  map[string]bool
	ConfigExisted bool
}

// keyDescription augments a provider field's static description with a note when
// the credential is already present in the environment / .env.local.
func keyDescription(spec ProviderKeySpec, existing map[string]bool) string {
	if existing[spec.EnvVar] {
		return spec.Description + " (already set — leave blank to keep)"
	}
	return spec.Description
}

// BuildForm constructs the huh form, binding each field to the provided
// pointers. Optional provider inputs live in a group hidden until the user
// confirms they want to configure more providers. Kept separate from Run so the
// field/grouping layout can be reasoned about (and the form built) apart from
// the interactive execution.
func BuildForm(
	keyValues map[string]*string,
	configureMore *bool,
	profile *string,
	dest *string,
	save *bool,
	in Input,
) *huh.Form {
	required := []huh.Field{}
	optional := []huh.Field{}
	for _, spec := range ProviderKeys {
		input := huh.NewInput().
			Title(spec.Title).
			Description(keyDescription(spec, in.ExistingKeys)).
			EchoMode(huh.EchoModePassword).
			Value(keyValues[spec.EnvVar])
		// Mistral is required: reject an empty value unless it is already set
		// (in which case a blank field means "keep the existing key").
		if spec.EnvVar == MistralEnvVar && !in.ExistingKeys[MistralEnvVar] {
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
		options = append(options, huh.NewOption("Keep current settings (don't change retrieval)", string(ProfileKeep)))
	}
	options = append(options,
		huh.NewOption("General documents", string(ProfileGeneral)),
		huh.NewOption("Legal / citations (strict grounding)", string(ProfileLegal)),
		huh.NewOption("Source code", string(ProfileCode)),
	)
	profileGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Corpus profile").
			Description("Tunes retrieval for this kind of content.").
			Options(options...).
			Value(profile),
	)

	destGroup := huh.NewGroup(
		huh.NewSelect[string]().
			Title("Where to store credentials?").
			Description("Keychain is encrypted at rest; .env.local also works for the background service.").
			Options(
				huh.NewOption(".env.local file", string(DestFile)),
				huh.NewOption("OS keychain (encrypted)", string(DestKeychain)),
			).
			Value(dest),
	)

	confirmGroup := huh.NewGroup(
		huh.NewConfirm().
			Title("Save these settings?").
			Affirmative("Save").
			Negative("Cancel").
			Value(save),
	)

	return huh.NewForm(providerGroup, optionalGroup, destGroup, profileGroup, confirmGroup).
		WithTheme(brandTheme())
}

// PromptSecret runs a single masked, brand-themed input and returns the entered
// value (trimmed). The caller must ensure stdin/stdout are a TTY. A cancelled
// prompt (Ctrl-C) returns huh.ErrUserAborted.
func PromptSecret(title, description string) (string, error) {
	var v string
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewInput().
				Title(title).
				Description(description).
				EchoMode(huh.EchoModePassword).
				Value(&v),
		),
	).WithTheme(brandTheme())
	if err := form.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(v), nil
}

// Confirm runs a brand-themed yes/no confirmation, returning the user's choice.
// The caller must ensure stdin/stdout are a TTY. A cancelled prompt (Ctrl-C)
// returns (false, huh.ErrUserAborted).
func Confirm(title, description string, defaultYes bool) (bool, error) {
	v := defaultYes
	form := huh.NewForm(
		huh.NewGroup(
			huh.NewConfirm().
				Title(title).
				Description(description).
				Affirmative("Yes").
				Negative("No").
				Value(&v),
		),
	).WithTheme(brandTheme())
	if err := form.Run(); err != nil {
		return false, err
	}
	return v, nil
}

// Run runs the interactive huh setup form and returns the user's answers.
// Credentials are returned (not yet written) so the caller controls persistence
// (.env.local) and config patching. Declining the final "Save" confirm surfaces
// as huh.ErrUserAborted so callers treat it like Ctrl-C.
func Run(in Input) (Result, error) {
	keyValues := make(map[string]*string, len(ProviderKeys))
	for _, spec := range ProviderKeys {
		keyValues[spec.EnvVar] = new(string)
	}
	var configureMore bool
	save := true
	profile := string(ProfileGeneral)
	if in.ConfigExisted {
		profile = string(ProfileKeep)
	}
	dest := string(DestFile)

	form := BuildForm(keyValues, &configureMore, &profile, &dest, &save, in)
	if err := form.Run(); err != nil {
		return Result{}, err
	}
	if !save {
		return Result{}, huh.ErrUserAborted
	}

	keys := make(map[string]string)
	for env, ptr := range keyValues {
		if v := strings.TrimSpace(*ptr); v != "" {
			keys[env] = v
		}
	}
	return Result{Keys: keys, Profile: Profile(profile), Destination: SecretDest(dest)}, nil
}

// PersistKeys writes each non-empty collected credential via write (a dotenv
// upsert), iterating in ProviderKeys order for deterministic writes, and returns
// the env-var names that were saved. The writer is injected so persistence is
// testable and so the CLI controls file location/permissions.
func PersistKeys(envPath string, keys map[string]string, write func(path, keyName, value string) error) ([]string, error) {
	saved := make([]string, 0, len(keys))
	for _, spec := range ProviderKeys {
		v, ok := keys[spec.EnvVar]
		if !ok || strings.TrimSpace(v) == "" {
			continue
		}
		if err := write(envPath, spec.EnvVar, strings.TrimSpace(v)); err != nil {
			return saved, err
		}
		saved = append(saved, spec.EnvVar)
	}
	return saved, nil
}

// DetectExistingKeys reports which wizard credentials already resolve, either
// from the process environment or from a non-empty assignment in .env.local at
// envPath. Used to relax the required check and annotate fields.
func DetectExistingKeys(envPath string) map[string]bool {
	out := make(map[string]bool, len(ProviderKeys))
	var dotenv string
	if b, err := os.ReadFile(envPath); err == nil {
		dotenv = string(b)
	}
	for _, spec := range ProviderKeys {
		if strings.TrimSpace(os.Getenv(spec.EnvVar)) != "" || DotenvHasKey(dotenv, spec.EnvVar) {
			out[spec.EnvVar] = true
		}
	}
	return out
}

// DotenvHasKey reports whether content has a non-empty assignment for key,
// honoring an optional leading "export " and treating quoted-empty values
// (KEY="" / KEY=”) as unset.
func DotenvHasKey(content, key string) bool {
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

// EnsureGitignoreEntries appends any missing entries to dir/.gitignore (creating
// it if needed), so credentials and local state are not accidentally committed.
// Existing entries and file content are preserved.
func EnsureGitignoreEntries(dir string, entries ...string) error {
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
