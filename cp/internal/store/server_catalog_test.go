package store

// The catalog is the only definition of a server type, so these tests are the
// only thing standing between an edit to it and a dashboard, a bill or an API
// boundary that quietly disagrees with the domain model.

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const generatedTSPath = "../../../web/src/lib/server-catalog.generated.ts"

// The one that matters: the checked-in TypeScript must be exactly what the
// current catalog renders. Without it, "adding a server type is a one-file
// edit" is only true for whoever remembers to run go generate — and the person
// who forgets ships a dashboard offering types the API rejects, which is the
// original SIGMA-198 defect wearing a new hat.
func TestGeneratedTypeScriptIsUpToDate(t *testing.T) {
	sha, err := CatalogSourceDigest(CatalogSourceFiles...)
	if err != nil {
		t.Fatalf("digest %v: %v", CatalogSourceFiles, err)
	}
	want := RenderTypeScript(sha)
	got, err := os.ReadFile(generatedTSPath)
	if err != nil {
		t.Fatalf("read %s: %v", generatedTSPath, err)
	}
	if string(got) == string(want) {
		return
	}
	// Point at the first differing line: a 12KB diff dump helps nobody.
	gotLines, wantLines := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gotLines) || i < len(wantLines); i++ {
		g, w := "", ""
		if i < len(gotLines) {
			g = gotLines[i]
		}
		if i < len(wantLines) {
			w = wantLines[i]
		}
		if g != w {
			t.Fatalf("%s is stale — run `cd cp && go generate ./...`\nline %d:\n  have: %s\n  want: %s",
				generatedTSPath, i+1, g, w)
		}
	}
}

// The generated module must actually carry the catalog, not merely parse. A
// renderer that silently dropped a section would still be byte-identical to
// itself, so the staleness test above cannot catch it.
func TestGeneratedTypeScriptCarriesTheCatalog(t *testing.T) {
	sha, err := CatalogSourceDigest(CatalogSourceFiles...)
	if err != nil {
		t.Fatal(err)
	}
	ts := string(RenderTypeScript(sha))

	if !strings.Contains(ts, `export const CATALOG_SOURCE_SHA256 = "`+sha+`";`) {
		t.Fatal("the source digest is missing; the web-side drift guard has nothing to compare")
	}
	for _, spec := range ServerCatalog() {
		for _, fragment := range []string{
			`  | "` + spec.Type + `"`,                    // the ServerType union
			`  ` + spec.Type + `: "` + spec.Label + `",`, // SERVER_TYPE_LABELS
			`  ` + spec.Type + `: "` + spec.Hint + `",`,  // SERVER_TYPE_HINTS
		} {
			if !strings.Contains(ts, fragment) {
				t.Errorf("server type %q: generated TS is missing %q", spec.Type, fragment)
			}
		}
		// Requirements are the half SIGMA-203 consumes; a type whose checks
		// never reached the dashboard would show an empty expectations list.
		for _, req := range spec.Requires.List() {
			if !strings.Contains(ts, `text: "`+req.Text+`"`) {
				t.Errorf("server type %q: requirement %q missing from generated TS", spec.Type, req.Text)
			}
		}
	}
	for _, kind := range ResourceKinds() {
		if !strings.Contains(ts, `  `+kind+`: "`+ResourceKindLabel(kind)+`",`) {
			t.Errorf("resource kind %q: label missing from generated TS", kind)
		}
	}
}

// The two directions of the matrix are one table read two ways. A transpose
// that lost an entry would let the deploy wizard offer a server CreateResource
// then refuses — the 422 this matrix exists to prevent.
func TestMatrixTransposeAgrees(t *testing.T) {
	for _, spec := range ServerCatalog() {
		for _, kind := range spec.Hosts {
			if !contains(AllowedServerTypes(kind), spec.Type) {
				t.Errorf("%s hosts %s, but AllowedServerTypes(%s) omits it", spec.Type, kind, kind)
			}
		}
	}
	for _, kind := range ResourceKinds() {
		for _, typ := range AllowedServerTypes(kind) {
			if !CanHost(typ, kind) {
				t.Errorf("AllowedServerTypes(%s) contains %s, but CanHost says otherwise", kind, typ)
			}
		}
	}
}

// AllowedServerTypes distinguishes "unknown kind" (nil) from "known kind
// nothing hosts" (empty). CreateResource branches on exactly that difference to
// choose between "unknown resource kind" and the matrix message, so collapsing
// the two would mislabel a real domain rule as a typo.
func TestAllowedServerTypesSeparatesUnknownFromUnhostable(t *testing.T) {
	if AllowedServerTypes("not-a-kind") != nil {
		t.Fatal("an unknown kind must return nil")
	}
	for _, kind := range ResourceKinds() {
		if AllowedServerTypes(kind) == nil {
			t.Fatalf("known kind %q returned nil, which reads as a typo to CreateResource", kind)
		}
	}
}

// Categories are what the wizard offers before it offers a kind. Membership is
// stated once, on the kind, so what is worth restating here is that the derived
// transpose really is a PARTITION: a kind in two buckets is rendered twice, and
// a kind in none is a kind nobody can reach through the wizard at all.
func TestEveryKindSitsInExactlyOneCategory(t *testing.T) {
	seen := make(map[string]int, len(ResourceKinds()))
	for _, c := range ResourceCategoryCatalog() {
		if c.Label == "" || c.Hint == "" {
			t.Errorf("category %q has no label or hint; its card would render blank", c.ID)
		}
		if len(c.Kinds) == 0 {
			t.Errorf("category %q holds no kinds; its card would open an empty list", c.ID)
		}
		for _, kind := range c.Kinds {
			if ResourceKindLabel(kind) == kind {
				t.Errorf("category %q holds %q, which is not a labelled kind", c.ID, kind)
			}
			seen[kind]++
		}
	}
	for _, kind := range ResourceKinds() {
		if seen[kind] != 1 {
			t.Errorf("kind %q is in %d categories; it must be in exactly one", kind, seen[kind])
		}
	}
}

// "This kind is a database" and "this kind has a managed engine" are meant to be
// one fact, and server_catalog.go's init panics if they ever stop being. A panic
// at package load cannot be asserted from inside the package it would refuse to
// load, so this states the property those panics defend — and states it through
// the PUBLISHED accessors, which is what every consumer actually reads.
//
// The ticket's phantom-kind proof was adding {"clickhouse", "ClickHouse",
// "database"} to resourceKinds: it compiled, regenerated and left every suite on
// both sides of the product green while nothing could provision it — the wizard
// offered a card the reconciler had no image, no port and no connection-URL
// shape for. This is the assertion it now fails.
func TestEveryDatabaseKindHasAnEngineAndEveryEngineIsADatabaseKind(t *testing.T) {
	var inCategory []string
	for _, c := range ResourceCategoryCatalog() {
		if c.ID == CategoryDatabase {
			inCategory = c.Kinds
		}
	}
	if len(inCategory) == 0 {
		t.Fatalf("the catalog holds no %q category, so CP_DB_ENGINES validates against nothing",
			CategoryDatabase)
	}
	if engines := DBEngineKinds(); !slices.Equal(engines, inCategory) {
		t.Fatalf("db_engines.go defines %v, the %q category holds %v — a kind in one and not the "+
			"other is either a managed database with no image or an engine no UI ever offers",
			engines, CategoryDatabase, inCategory)
	}
	// …and the per-kind predicate the create path branches on has to answer the
	// same question, so IsDBKind cannot claim a kind the wizard files elsewhere.
	for _, kind := range ResourceKinds() {
		if want := slices.Contains(inCategory, kind); IsDBKind(kind) != want {
			t.Errorf("IsDBKind(%q) = %v but the %q category holding it is %v; they are one question",
				kind, IsDBKind(kind), CategoryDatabase, want)
		}
	}
}

// Both orders the picker renders in are the catalog's own: the categories in
// the order they are written down, and the kinds within one in the order the
// kind list is written down. Nothing sorts either at the point of display, so
// this file is where "what comes first" is decided.
func TestCategoryOrderFollowsTheCatalog(t *testing.T) {
	ids := ResourceCategories()
	catalog := ResourceCategoryCatalog()
	if len(ids) != len(catalog) {
		t.Fatalf("ResourceCategories lists %d, ResourceCategoryCatalog lists %d", len(ids), len(catalog))
	}
	for i, c := range catalog {
		if c.ID != ids[i] {
			t.Errorf("position %d: ResourceCategories says %q, the catalog says %q", i, ids[i], c.ID)
		}
	}
	kinds := ResourceKinds()
	for _, c := range catalog {
		last := -1
		for _, kind := range c.Kinds {
			at := indexOf(kinds, kind)
			if at <= last {
				t.Errorf("category %q lists %q out of catalog order", c.ID, kind)
			}
			last = at
		}
	}
}

// The categories reach the dashboard the same way everything else does. A
// category that never rendered would leave the picker with a card holding no
// kinds — which the web cannot distinguish from a category we deliberately
// emptied, because we never can: the catalog refuses to hold one.
func TestGeneratedTypeScriptCarriesTheCategories(t *testing.T) {
	sha, err := CatalogSourceDigest(CatalogSourceFiles...)
	if err != nil {
		t.Fatal(err)
	}
	ts := string(RenderTypeScript(sha))

	for _, c := range ResourceCategoryCatalog() {
		for _, fragment := range []string{
			`  | "` + c.ID + `"`, // the ResourceCategoryId union
			// …and the three fields of its RESOURCE_CATEGORY_CATALOG entry.
			`    label: ` + tsString(c.Label) + `,`,
			`    hint: ` + tsString(c.Hint) + `,`,
			`    kinds: ` + tsStringArray(c.Kinds) + `,`,
		} {
			if !strings.Contains(ts, fragment) {
				t.Errorf("category %q: generated TS is missing %q", c.ID, fragment)
			}
		}
	}
	for _, k := range resourceKinds {
		if fragment := `  ` + k.Kind + `: "` + k.Category + `",`; !strings.Contains(ts, fragment) {
			t.Errorf("kind %q: KIND_CATEGORY is missing %q", k.Kind, fragment)
		}
	}
}

// The kinds a cluster refuses are published TWICE — by the API at runtime
// (GET /clusters returns excludedKinds) and by the generated catalog, which the
// dashboard compiles in for demo mode, where there is no control plane to ask.
// Two publishers of one rule is the drift this test exists to forbid: while the
// demo hard-coded an empty list, clusterEligible() answered true for every kind,
// so a demo cluster would have been offered as a target for a Postgres that the
// real product refuses — the two sides of one question disagreeing, which is the
// defect single-sourcing the catalog was meant to end. Rendering FROM
// ClusterExcludedKinds is what makes them one statement; this asserts it, so the
// day someone hand-writes the TS array the CP suite says so.
func TestBothPublishedClusterExclusionListsAreOneStatement(t *testing.T) {
	sha, err := CatalogSourceDigest(CatalogSourceFiles...)
	if err != nil {
		t.Fatal(err)
	}
	ts := string(RenderTypeScript(sha))

	published := ClusterExcludedKinds()
	fragment := "export const CLUSTER_EXCLUDED_KINDS: ResourceKind[] = " + tsStringArray(published) + ";"
	if !strings.Contains(ts, fragment) {
		t.Fatalf("the generated catalog does not carry the API's own list %v;\nexpected: %s",
			published, fragment)
	}

	// …and the published list must be the rule the create call actually
	// enforces. A kind listed but allowed sends the wizard to a target that
	// works; a kind excluded but unlisted sends it to one that 422s after Review.
	for _, kind := range published {
		if ClusterKindAllowed(kind) {
			t.Errorf("%q is published as excluded but ClusterKindAllowed accepts it", kind)
		}
	}
	for _, kind := range ResourceKinds() {
		if !ClusterKindAllowed(kind) && !contains(published, kind) {
			t.Errorf("%q is refused inside a cluster but is not published, so the wizard offers it", kind)
		}
	}
	// The rule itself, so a one-word edit to the map cannot reverse it quietly.
	if !ClusterKindAllowed("app") {
		t.Error("app must be deployable into a cluster; it is what clusters are for")
	}
	for _, kind := range []string{"postgres", "mysql", "mongodb", "redis", "s3", "llm"} {
		if ClusterKindAllowed(kind) {
			t.Errorf("%q must not run inside a cluster", kind)
		}
	}
}

// What a managed engine IS — its pinned image and the shape of the connection
// string it hands out — is published twice for the same reason the cluster
// exclusions are: the API answers with it at runtime, and the dashboard
// compiles it in for demo mode, where there is no control plane to ask. Demo
// mode used to answer from a table of its own, and every single value in it had
// drifted: postgres:17-alpine against this package's 16.6 (a MAJOR version a
// customer plans around), mysql:8.4 for 8.4.4, mongo:8 for 7.0.16,
// redis:7-alpine for 7.4.2, and two floating "latest" tags the agent's image
// policy refuses outright. Both panels print the value under a label reading
// "Engine", so this asserts the rendered module carries THIS catalog.
func TestGeneratedTypeScriptCarriesTheEngineCatalogs(t *testing.T) {
	sha, err := CatalogSourceDigest(CatalogSourceFiles...)
	if err != nil {
		t.Fatal(err)
	}
	ts := string(RenderTypeScript(sha))

	for _, def := range DBEngineCatalog() {
		for _, fragment := range []string{
			`  | "` + def.Engine + `"`, // the DatabaseEngine union
			`    image: ` + tsString(def.Image) + `,`,
			`    urlTemplate: ` + tsString(def.URLTemplate) + `,`,
		} {
			if !strings.Contains(ts, fragment) {
				t.Errorf("db engine %q: generated TS is missing %q", def.Engine, fragment)
			}
		}
	}
	for _, def := range S3EngineCatalog() {
		for _, fragment := range []string{
			`  | "` + def.Engine + `"`,
			`    image: ` + tsString(def.Image) + `,`,
			`    endpointTemplate: ` + tsString(def.EndpointTemplate) + `,`,
		} {
			if !strings.Contains(ts, fragment) {
				t.Errorf("s3 engine %q: generated TS is missing %q", def.Engine, fragment)
			}
		}
	}
	// The port is the other half of the disagreement: demo mode printed the
	// engines' CONTAINER ports, and this product publishes a managed engine on
	// a port allocated per server from here up.
	if fragment := fmt.Sprintf("export const MESH_PORT_BASE = %d;", MeshPortBase); !strings.Contains(ts, fragment) {
		t.Errorf("the generated catalog does not carry the allocator's port base;\nexpected: %s", fragment)
	}
	if strings.Contains(ts, "containerPort") {
		t.Error("the generated catalog publishes a container port; nothing outside the container dials one, and publishing it is how a panel comes to print 5432 for a database reachable on 15000+")
	}
}

// The inference runtimes are published twice for the same reason the engine
// catalogs are: GET /llm/engines answers with them at runtime, and the wizard
// compiles them in — it has to send an engine for a model whose card did not
// resolve, before any list has been fetched. That copy was hand-written, so
// renaming or replacing the default runtime here left the wizard sending
// "vllm" and provisionLLMTx answering `unknown inference runtime "vllm"` as a
// 422 at the end of the LLM wizard, with every Go and TypeScript suite green
// (SIGMA-278).
//
// The order matters as much as the membership: the names are rendered into a
// checked-in file, so an order that came from a Go map would have made the
// generator emit a different file on every run.
func TestGeneratedTypeScriptCarriesTheRuntimeCatalog(t *testing.T) {
	sha, err := CatalogSourceDigest(CatalogSourceFiles...)
	if err != nil {
		t.Fatal(err)
	}
	ts := string(RenderTypeScript(sha))

	names := LLMEngineNames()
	if len(names) != len(llmEngines) {
		t.Fatalf("llmEngineOrder names %d runtimes, llmEngines holds %d — an engine "+
			"missing from the order is invisible to the API and to the dashboard", len(names), len(llmEngines))
	}
	for _, fragment := range []string{
		"export const LLM_ENGINE_NAMES: LLMEngine[] = " + tsStringArray(names) + ";",
		"export const DEFAULT_LLM_ENGINE: LLMEngine = " + tsString(DefaultLLMEngine) + ";",
	} {
		if !strings.Contains(ts, fragment) {
			t.Errorf("the generated catalog does not carry the runtime catalog;\nexpected: %s", fragment)
		}
	}
	for _, name := range names {
		if !IsLLMEngine(name) {
			t.Errorf("runtime %q is published but IsLLMEngine refuses it", name)
		}
		if fragment := `  | "` + name + `"`; !strings.Contains(ts, fragment) {
			t.Errorf("runtime %q is missing from the LLMEngine union", name)
		}
	}
	// The default is the value the wizard sends when nothing else is known, so
	// it has to be a runtime this package can actually render.
	if !IsLLMEngine(DefaultLLMEngine) {
		t.Fatalf("DefaultLLMEngine %q is not a runtime in the catalog", DefaultLLMEngine)
	}
}

// Every image this control plane pins must be one the AGENT will accept:
// agent/internal/container/policy.go refuses a bare repository and refuses the
// floating "latest" tag ("pin a version tag or digest"), so an unpinned image
// here is a resource that provisions and then fails at container create — and,
// once the catalog reaches the dashboard, an image the demo advertises that the
// product would decline to run.
func TestEveryEngineImageIsPinnedTheWayTheAgentDemands(t *testing.T) {
	images := map[string]string{}
	for _, def := range DBEngineCatalog() {
		images[def.Engine] = def.Image
	}
	for _, def := range S3EngineCatalog() {
		images[def.Engine] = def.Image
	}
	if len(images) == 0 {
		t.Fatal("no engines in the catalog")
	}
	for engine, image := range images {
		if strings.Contains(image, "@sha256:") {
			continue // digest-pinned: immutable, the ideal form
		}
		i := strings.LastIndex(image, ":")
		if i < 0 || strings.Contains(image[i+1:], "/") {
			t.Errorf("%s: image %q carries no tag; the agent refuses it", engine, image)
			continue
		}
		if tag := image[i+1:]; tag == "latest" {
			t.Errorf("%s: image %q floats; the agent refuses the latest tag", engine, image)
		}
	}
}

// The connection URL is the one thing here that is a FUNCTION rather than a
// table, and it is the reason the shapes are templates: the dashboard renders
// the same string in demo mode, and its own switch statement had grown an
// ?sslmode=disable Postgres never gets from us and a MongoDB URL with the
// database in the path — where this catalog authenticates on admin and puts no
// database there at all. One template, filled on both sides.
func TestConnectionURLRendersFromTheEngineTemplate(t *testing.T) {
	for _, tc := range []struct {
		kind string
		want string
	}{
		{"postgres", "postgresql://sigma:pw@10.8.0.21:15003/orders"},
		{"mysql", "mysql://sigma:pw@10.8.0.21:15003/orders"},
		{"redis", "redis://:pw@10.8.0.21:15003/0"},
		{"mongodb", "mongodb://sigma:pw@10.8.0.21:15003/?authSource=admin"},
	} {
		def, ok := DBEngine(tc.kind)
		if !ok {
			t.Fatalf("%s is not a database engine", tc.kind)
		}
		if got := def.ConnectionURL("sigma", "pw", "10.8.0.21", 15003, "orders"); got != tc.want {
			t.Errorf("%s URL = %q, want %q", tc.kind, got, tc.want)
		}
	}
	// One pass: a credential that happens to contain a placeholder must be
	// carried through as itself, never re-read. The TypeScript half does the
	// same, which is what keeps one template one string.
	def, _ := DBEngine("postgres")
	if got := def.ConnectionURL("sigma", "{host}", "10.8.0.21", 15003, "orders"); got != "postgresql://sigma:{host}@10.8.0.21:15003/orders" {
		t.Errorf("a password containing a placeholder was re-substituted: %q", got)
	}
	// An engine nothing in the catalog describes renders nothing, rather than a
	// URL shaped like a guess at what it might have been.
	if got := (DBEngineDef{Engine: "cassandra"}).ConnectionURL("u", "p", "h", 1, "d"); got != "" {
		t.Errorf("an unknown engine rendered %q", got)
	}
}

// The S3 endpoint is the same story one layer along: the port in it is the
// ALLOCATED mesh port, not the engine's API port, and demo mode printed 9000 —
// MinIO's in-container port, and not SeaweedFS's at all.
func TestS3EndpointRendersOnTheAllocatedPort(t *testing.T) {
	for _, def := range S3EngineCatalog() {
		if got, want := def.EndpointURL("10.8.0.9", 15007), "http://10.8.0.9:15007"; got != want {
			t.Errorf("%s endpoint = %q, want %q", def.Engine, got, want)
		}
		if def.APIPort == 0 {
			t.Errorf("%s has no API port; the container render needs one", def.Engine)
		}
		// A host with no mesh address yet has no endpoint at all — the panel
		// says "not enrolled yet" rather than offering a URL to nowhere.
		if got := def.EndpointURL("", 15007); got != "" {
			t.Errorf("%s rendered an endpoint for a host with no mesh address: %q", def.Engine, got)
		}
	}
}

// The exclusion list is rendered into a CHECKED-IN file, so its order has to be
// a property of the catalog rather than of a map range: Go randomizes map
// iteration, and a list that reshuffles per run makes `go generate` produce a
// different file most times it runs — the staleness test would then fail on
// commits that changed nothing.
func TestClusterExclusionsAreListedInCatalogOrder(t *testing.T) {
	kinds := ResourceKinds()
	last := -1
	for _, kind := range ClusterExcludedKinds() {
		at := indexOf(kinds, kind)
		if at < 0 {
			t.Fatalf("%q is published as excluded but is not a known resource kind", kind)
		}
		if at <= last {
			t.Errorf("%q is out of catalog order; the rendered TypeScript is not reproducible", kind)
		}
		last = at
	}
}

// Domain rules worth restating as tests: each was a deliberate product
// decision, and each would be silently reversible by a one-word catalog edit.
func TestCatalogDomainRules(t *testing.T) {
	if !equalSets(hostsOf(t, "vps"), hostsOf(t, "general")) {
		t.Error("a VPS must host exactly what a general server hosts — virtualization is a disclosure, not a capability difference")
	}
	for _, typ := range ServerTypes() {
		if CanHost(typ, "llm") != (typ == "gpu") {
			t.Errorf("llm on %q = %v; models are served on GPU hardware only", typ, CanHost(typ, "llm"))
		}
		if CanHost(typ, "s3") != (typ == "storage") {
			t.Errorf("s3 on %q = %v; object storage belongs on a storage host", typ, CanHost(typ, "s3"))
		}
	}
	for _, kind := range []string{"postgres", "mysql", "mongodb", "redis"} {
		if CanHost("storage", kind) || CanHost("gpu", kind) {
			t.Errorf("%s must not land on a storage or gpu server", kind)
		}
	}
	for _, typ := range []string{"k8s", "build"} {
		spec, ok := ServerTypeSpecFor(typ)
		if !ok {
			t.Fatalf("%s is not in the catalog", typ)
		}
		if len(spec.Hosts) != 0 {
			t.Errorf("%s must host nothing directly", typ)
		}
		if spec.HostsNothingReason == "" {
			t.Errorf("%s hosts nothing and does not say why — the UI would show a blank dead end", typ)
		}
	}
	// k8s is deliberately absent from the connect dialog: a node becomes one by
	// JOINING a cluster, so offering it at enrollment creates a host nothing can
	// be scheduled onto that nevertheless bills at cluster weight.
	if contains(ConnectableServerTypes(), "k8s") {
		t.Error("k8s must not be offered when connecting a new server")
	}
	// ...but it is still a canonical type, and the API boundary accepts every
	// canonical type. That asymmetry is the point: guidance in the UI, not a
	// second, narrower list at the edge.
	if !IsServerType("k8s") {
		t.Error("k8s must remain a known server type")
	}
}

// Every type states its own requirements. A type that inherits silence would
// pass SIGMA-203's gate unconditionally — the failure mode there is a host
// enrolled as something it cannot be, discovered at first deploy.
func TestEveryTypeStatesItsRequirements(t *testing.T) {
	for _, spec := range ServerCatalog() {
		req, ok := ServerRequirementsFor(spec.Type)
		if !ok {
			t.Fatalf("%s: no requirements", spec.Type)
		}
		if len(req.Distros) == 0 {
			t.Errorf("%s: no supported distros", spec.Type)
		}
		if len(req.Arches) == 0 {
			t.Errorf("%s: no supported architectures", spec.Type)
		}
		for _, d := range req.Distros {
			if !DistroSupported(d) {
				t.Errorf("%s: requires distro %q that the onboarding path rejects", spec.Type, d)
			}
		}
		// Every requirement must render a sentence with the fact it reads, or
		// the gate can only answer "no" without saying what to change.
		for _, check := range req.List() {
			if check.Text == "" || check.Fact == "" {
				t.Errorf("%s: requirement %q has no text or no fact", spec.Type, check.ID)
			}
			if !strings.HasSuffix(check.Text, ".") {
				t.Errorf("%s: requirement %q is not a sentence: %q", spec.Type, check.ID, check.Text)
			}
		}
	}

	// The requirements that carry real product meaning.
	gpu, _ := ServerRequirementsFor("gpu")
	if gpu.GPU == nil || gpu.GPU.Vendor != "nvidia" || !gpu.GPU.Driver {
		t.Error("a gpu server must require an NVIDIA GPU with a usable driver")
	}
	if !strings.Contains(requirementText(gpu, ReqGPU), "usable driver") {
		t.Errorf("the GPU requirement must name the driver: %q", requirementText(gpu, ReqGPU))
	}
	for _, typ := range []string{"database", "storage"} {
		req, _ := ServerRequirementsFor(typ)
		if req.MinDiskBytes <= 0 {
			t.Errorf("%s: needs a minimum disk floor", typ)
		}
		if requirementText(req, ReqDisk) == "" {
			t.Errorf("%s: the disk floor is not stated in words", typ)
		}
	}
	if s, _ := ServerRequirementsFor("storage"); s.MinDiskBytes <= mustDisk(t, "database") {
		t.Error("a storage host must need more disk than a database host — capacity is the whole promise of the type")
	}
	// Types with no accelerator story must not accidentally demand one.
	for _, typ := range ServerTypes() {
		req, _ := ServerRequirementsFor(typ)
		if req.GPU != nil && typ != "gpu" {
			t.Errorf("%s requires a GPU; only the gpu type should", typ)
		}
	}
}

// The billing weight is a catalog field precisely so it cannot go missing: vps
// and build were absent from the old standalone weight map and billed at the
// fallback by accident.
func TestEveryTypeHasABillingWeight(t *testing.T) {
	weights := ServerUnitWeights()
	for _, typ := range ServerTypes() {
		w, ok := weights[typ]
		if !ok || w <= 0 {
			t.Errorf("%s has no billing weight", typ)
		}
		if got := ServerUnitWeight(typ); got != w {
			t.Errorf("%s: ServerUnitWeight = %d, table says %d", typ, got, w)
		}
	}
	if len(weights) != len(ServerTypes()) {
		t.Errorf("the weight table has %d entries for %d types", len(weights), len(ServerTypes()))
	}
	if ServerUnitWeight("totally-unknown") != DefaultServerUnitWeight {
		t.Error("an unknown type must bill as an ordinary server, never as free")
	}
	// The SQL the drift sweep runs is generated from the same table; a type
	// missing from it bills at the fallback inside the database only, which is
	// invisible until an invoice disagrees with the dashboard.
	sql := unitWeightSQL("s.type")
	for _, typ := range ServerTypes() {
		if !strings.Contains(sql, "WHEN '"+typ+"'") {
			t.Errorf("unitWeightSQL omits %q: %s", typ, sql)
		}
	}
}

// billingDocPath is the repository's ONLY prose description of pricing.
const billingDocPath = "../../docs/billing.md"

// TestBillingDocMatchesPricingConstants is SIGMA-296. The doc described the
// pre-units model — "one flat unit (€5) per connected SERVER per month, with the
// first 3 connected servers free", and a billable count of `max(0, connected -
// 3)` — long after the billed quantity became weighted UNITS. An operator or a
// design partner pricing three GPU servers off it computed €0 (three servers,
// all free) against an actual charge of 12 units − 3 free = 9 × €5 = €45/month.
// A 4x miss in the customer's favour is a refund conversation; in front of a
// prospect it is a misrepresentation of price during a sale.
//
// The pricing constants and the web read model are already held together by a
// test. This adds the third party to that agreement, so the prose cannot drift
// alone. The doc states its numbers in tables precisely so this can read them.
func TestBillingDocMatchesPricingConstants(t *testing.T) {
	raw, err := os.ReadFile(billingDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", billingDocPath, err)
	}
	doc := string(raw)

	one := func(pattern, what string) string {
		m := regexp.MustCompile(pattern).FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("%s does not state the %s in a form this test can read (pattern %s)",
				billingDocPath, what, pattern)
		}
		return m[1]
	}
	if got := one(`\|\s*Unit price\s*\|\s*€(\d+)`, "unit price"); got != strconv.Itoa(BillingUnitPrice) {
		t.Errorf("the doc prices a unit at €%s; billing.go charges €%d", got, BillingUnitPrice)
	}
	if got := one(`\|\s*Free tier\s*\|\s*(\d+) units`, "free tier"); got != strconv.Itoa(BillingFreeTier) {
		t.Errorf("the doc gives away %s units; billing.go gives away %d", got, BillingFreeTier)
	}
	if got := one(`\|\s*Currency\s*\|\s*([A-Z]{3})`, "currency"); got != BillingCurrency {
		t.Errorf("the doc bills in %s; billing.go bills in %s", got, BillingCurrency)
	}

	// The weight table, which is the half the old doc did not have at all — and
	// its absence is exactly what made the GPU example wrong.
	documented := map[string]int{}
	for _, m := range regexp.MustCompile("(?m)^\\|\\s*`([a-z0-9_]+)`\\s*\\|\\s*(\\d+)\\s*\\|").FindAllStringSubmatch(doc, -1) {
		n, _ := strconv.Atoi(m[2])
		documented[m[1]] = n
	}
	weights := ServerUnitWeights()
	for typ, want := range weights {
		got, ok := documented[typ]
		if !ok {
			t.Errorf("server type %q has no documented weight; a reader pricing a fleet of them guesses", typ)
			continue
		}
		if got != want {
			t.Errorf("the doc weighs %q at %d units; the catalog weighs it at %d", typ, got, want)
		}
	}
	for typ := range documented {
		if _, ok := weights[typ]; !ok {
			t.Errorf("the doc documents a weight for %q, which is not a server type", typ)
		}
	}

	// And the formula the old doc stated, which counted SERVERS. Left in prose it
	// is worse than no formula: it looks authoritative and is off by the weights.
	for _, stale := range []string{"connected - 3", "connected servers free", "per **connected server**"} {
		if strings.Contains(doc, stale) {
			t.Errorf("%s still describes the pre-units model (%q)", billingDocPath, stale)
		}
	}
}

// The distro sentence is generated so the two rejection messages in the API
// cannot drift from the list they are describing.
func TestSupportedDistroSentence(t *testing.T) {
	got := SupportedDistroSentence()
	for _, d := range SupportedDistros() {
		if !DistroSupported(d) {
			t.Errorf("%q is listed but not supported", d)
		}
	}
	if !strings.Contains(got, " or ") {
		t.Errorf("the sentence must read as a list of alternatives: %q", got)
	}
	if strings.Contains(got, "ubuntu-22.04") {
		t.Errorf("the operator-facing sentence must use labels, not ids: %q", got)
	}
	if DistroSupported("centos-7") {
		t.Error("an unlisted distro must not be onboardable")
	}
}

func requirementText(r ServerRequirements, id RequirementID) string {
	for _, check := range r.List() {
		if check.ID == id {
			return check.Text
		}
	}
	return ""
}

func mustDisk(t *testing.T, typ string) int64 {
	t.Helper()
	req, ok := ServerRequirementsFor(typ)
	if !ok {
		t.Fatalf("%s is not in the catalog", typ)
	}
	return req.MinDiskBytes
}

// hostsOf reads a type's hosted kinds, failing the test if the type is unknown
// — a typo in a test fixture must not read as an empty host list.
func hostsOf(t *testing.T, typ string) []string {
	t.Helper()
	spec, ok := ServerTypeSpecFor(typ)
	if !ok {
		t.Fatalf("%s is not in the catalog", typ)
	}
	return spec.Hosts
}

func contains(items []string, want string) bool {
	return indexOf(items, want) >= 0
}

func indexOf(items []string, want string) int {
	for i, item := range items {
		if item == want {
			return i
		}
	}
	return -1
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !contains(b, x) {
			return false
		}
	}
	return true
}

// An engine can satisfy every catalog guard and still start a container nobody
// can log into.
//
// PlainEnv and TunedCommand are per-engine switch statements, which makes their
// case lists a second spelling of the dbEngines keys that nothing ties to the
// first. Adversarial review added a fully wired-up engine — a kind, a category,
// a catalog entry, all three init guards satisfied — and both methods fell
// through to nil: the reconciler rendered a container with no credentials env
// and no start command, while the connection panel printed a URL naming a user
// the engine had never been told to create.
//
// A switch cannot be made exhaustive in Go, so this is the substitute: the
// property each arm exists to provide, asserted for every engine the catalog
// knows. It fails on the edit that adds an engine, not on the deploy that
// reveals it.
func TestEveryEngineIsStartedWithCredentialsAndACommand(t *testing.T) {
	for _, def := range DBEngineCatalog() {
		t.Run(def.Engine, func(t *testing.T) {
			// Both tuning profiles: the production arm is reached only on a
			// database-type server, so an engine handled in one and not the
			// other would pass a test that checked a single profile.
			for _, serverType := range []string{"database", "general"} {
				if len(def.TunedCommand(serverType)) == 0 {
					t.Errorf("TunedCommand(%q) is empty: the container starts on the image's own "+
						"entrypoint, with none of the tuning this catalog exists to apply",
						serverType)
				}
			}
			// Redis is the one engine with no user or database concept, and it
			// says so by name in PlainEnv rather than by falling off the end.
			if def.Engine == "redis" {
				return
			}
			env := def.PlainEnv("sigma_user", "sigma_db")
			if len(env) == 0 {
				t.Fatalf("PlainEnv is empty: the engine is started with no credentials, and the " +
					"connection panel then prints a username nothing created")
			}
			var carriesUser bool
			for _, v := range env {
				if v == "sigma_user" {
					carriesUser = true
				}
			}
			if !carriesUser {
				t.Errorf("PlainEnv = %v, which never names the username it was given", env)
			}
		})
	}
}
