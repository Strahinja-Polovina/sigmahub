package gitdetect

// The auto-build fallback for a repository that describes no build of its own.
//
// Before this, a repo with neither a Dockerfile nor a Compose file was simply
// refused: "not deployable". That answer is wrong about the world — the vast
// majority of application repositories describe how to RUN themselves
// (package.json, go.mod, requirements.txt) and never describe how to
// containerize themselves, because until they meet a PaaS they never had to.
//
// # Nixpacks, not Cloud Native Buildpacks
//
// Both turn a source tree into an OCI image without a Dockerfile. We use
// nixpacks, for reasons that are about THIS agent rather than about which is
// the better technology:
//
//   - Nixpacks is a single static binary that reads a source directory and
//     drives `docker build`. It slots into the op we already have —
//     image.build, with a context directory and a tag — and needs no new op
//     kind, no daemon, and no privileged surface. Buildpacks need the `pack`
//     CLI plus a lifecycle that runs as its own set of containers with the
//     Docker socket bind-mounted, which is a materially larger thing to hand a
//     customer's machine than "one more binary that shells out to docker".
//   - A buildpacks build needs a BUILDER IMAGE chosen up front (heroku/builder,
//     paketo full/base/tiny …), ~1 GB, pinned and refreshed by us, and picking
//     the wrong one silently drops a language. Nixpacks resolves its toolchain
//     per repo from its own provider set, so adding a language is their release,
//     not our image-pinning decision.
//   - Its provider list maps one-to-one onto the marker files below, so what
//     the control plane TELLS the user we detected is the same evidence the
//     builder will use. Buildpacks' detect phase runs inside the lifecycle on
//     the target host, so the CP could only guess at it here and would
//     occasionally be wrong in the one place the user is being asked to decide.
//
// The trade we accept: nixpacks produces a larger image than a tuned Dockerfile
// and gives less control over the base layer. That is why the wizard always
// offers "switch to Dockerfile" alongside it, and why we hand out a starter
// Dockerfile for the language we detected.

// Build methods a repository can be built by. These strings cross the wire to
// the dashboard, into the stored resource spec and back down into the agent's
// image.build op, so they are a contract, not labels.
const (
	BuildDockerfile = "dockerfile"
	BuildCompose    = "compose"
	BuildNixpacks   = "nixpacks"
)

// ValidBuildMethod reports whether a string names a build method we know how to
// execute. The create path refuses anything else rather than storing a spec the
// renderer would silently reduce to "Dockerfile at the context root".
func ValidBuildMethod(m string) bool {
	switch m {
	case BuildDockerfile, BuildCompose, BuildNixpacks:
		return true
	}
	return false
}

// LanguageDef is one language the nixpacks fallback can build, identified by
// files that a project of that language essentially always has at its root.
type LanguageDef struct {
	ID    string
	Label string
	// Markers identify the language by PRESENCE alone. They are deliberately
	// manifest files (package.json, go.mod) rather than source extensions: a
	// repo with one .py script is not a Python service, and a repo with a
	// pyproject.toml is.
	Markers []string
	// DefaultPort is the port the language's conventional server listens on. A
	// repo with no Dockerfile has no EXPOSE either, so without this the deploy
	// would come out with no ports at all and its health probe would target
	// nothing (the SIGMA-160 failure, arrived at from the other direction).
	DefaultPort int
}

// languages is ordered: the FIRST match wins, so a repo that carries markers
// for two languages resolves deterministically. Deno precedes Node because a
// Deno project may also carry a package.json for editor tooling while nixpacks
// (and the runtime) treat it as Deno.
var languages = []LanguageDef{
	{ID: "deno", Label: "Deno", Markers: []string{"deno.json", "deno.jsonc"}, DefaultPort: 8000},
	{ID: "node", Label: "Node.js", Markers: []string{"package.json"}, DefaultPort: 3000},
	{ID: "go", Label: "Go", Markers: []string{"go.mod"}, DefaultPort: 8080},
	{ID: "python", Label: "Python", Markers: []string{"pyproject.toml", "requirements.txt", "Pipfile"}, DefaultPort: 8000},
	{ID: "ruby", Label: "Ruby", Markers: []string{"Gemfile"}, DefaultPort: 3000},
	{ID: "php", Label: "PHP", Markers: []string{"composer.json"}, DefaultPort: 8080},
	{ID: "rust", Label: "Rust", Markers: []string{"Cargo.toml"}, DefaultPort: 8080},
	{ID: "java", Label: "Java", Markers: []string{"pom.xml", "build.gradle", "build.gradle.kts"}, DefaultPort: 8080},
	{ID: "elixir", Label: "Elixir", Markers: []string{"mix.exs"}, DefaultPort: 4000},
	{ID: "dotnet", Label: ".NET", Markers: []string{"global.json"}, DefaultPort: 8080},
}

// LanguageMarkerNames lists every marker file name, for the inspector to know
// which paths are worth fetching. Presence is all that is read, so the
// inspector may record these as empty — see Detect's use of the name set.
func LanguageMarkerNames() []string {
	out := make([]string, 0, 16)
	for _, l := range languages {
		out = append(out, l.Markers...)
	}
	return out
}

// DetectLanguage picks the language a set of file names belongs to. names holds
// BASE names within a single directory, not repo paths.
func DetectLanguage(names map[string]bool) (LanguageDef, bool) {
	for _, l := range languages {
		for _, m := range l.Markers {
			if names[m] {
				return l, true
			}
		}
	}
	return LanguageDef{}, false
}

// Language returns a language definition by id, for callers rendering the
// detected result (the dashboard's starter-Dockerfile offer).
func Language(id string) (LanguageDef, bool) {
	for _, l := range languages {
		if l.ID == id {
			return l, true
		}
	}
	return LanguageDef{}, false
}
