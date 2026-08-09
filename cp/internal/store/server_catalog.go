package store

// The server-type catalog: the ONE place a server type is written down.
//
// Before SIGMA-198 the same enumeration existed five times — `serverTypes` and
// `resourceServerTypes` here, `validServerTypes` at the HTTP boundary,
// `serverUnitWeights` in billing, and a hand-maintained mirror in the
// dashboard. They disagreed: the API rejected `vps`, `k8s` and `build` that the
// store accepted, so the connect dialog offered VPS and Build buttons that
// answered 400, and `vps`/`build` billed at the fallback weight because nobody
// remembered the fourth copy existed.
//
// Everything a server type means now hangs off one entry in `serverCatalog`
// below: what it is called, what it may host, what it costs, whether it is
// offered at connect time, and what a host must actually HAVE to enroll as one.
// Adding a type — or a matrix entry, or a new per-type requirement — is an edit
// to this file plus `go generate ./...`, which renders the dashboard's copy
// from it (see server_catalog_ts.go) so the two cannot drift.
//
// It is not literally a one-file change, and the comment used to claim it was:
// a new type also needs an entry anywhere the web keys a table on ServerType
// (today, the demo capacity table in server/actions/servers.ts). That is by
// design rather than an oversight — those tables are `Record<ServerType, …>`,
// so tsc names the missing key and the file at compile time. A type list served
// at runtime could not do that, which is why this is codegen and not an
// endpoint.
//
// The resource-KIND vocabulary is not single-sourced to the same standard yet:
// adding a kind here compiles and passes everything while the two DB_KINDS
// tables, config.go's engine list and clusters.go's exclusions all stay
// ignorant of it (SIGMA-216). The wizard's PICKER is no longer on that list —
// its grouping, its labels and the order it offers them in are rendered from
// resourceCategories below — but the icon it draws beside each kind still lives
// in the dashboard, because a lucide component is not a thing this file can
// name.

//go:generate go run ../../cmd/gen-server-catalog

import (
	"fmt"
	"sort"
	"strings"
)

// RequirementID names one machine-checkable precondition. The ids are stable
// because SIGMA-203's registration gate returns them to the API caller and the
// dashboard keys its "here is how to fix it" copy on them — a renamed id is a
// silently blank remediation panel, not a compile error.
type RequirementID string

const (
	ReqDistro RequirementID = "distro"
	ReqArch   RequirementID = "arch"
	ReqDisk   RequirementID = "disk"
	ReqGPU    RequirementID = "gpu"
)

// GPURequirement is "this type needs real accelerator hardware".
//
// Vendor is matched against the GPU inventory the agent reports (SIGMA-201).
// Driver matters independently: a card with no usable kernel driver enumerates
// over PCI and then fails at the first container start, which is the worst
// possible moment to discover it — the host is already enrolled, already
// billed at GPU weight, and the operator is looking at a failed deploy rather
// than at the enrollment that should have refused.
type GPURequirement struct {
	Vendor string `json:"vendor"`
	Driver bool   `json:"driver"`
}

// ServerRequirements is what a host must have before it may enroll AS a given
// type. Deliberately data rather than a pile of if-statements at the
// registration handler: SIGMA-203 walks these fields to build its gate, the
// connect dialog renders them as expectations BEFORE the operator spends ten
// minutes on an install that was never going to be accepted, and both read the
// same sentences because both come from List().
//
// A zero field means "no constraint of this kind", so a new requirement is a
// new field plus a case in List() — never a new branch somewhere in the API.
type ServerRequirements struct {
	// Distros / Arches are allow-lists, never empty: every type states what it
	// runs on explicitly rather than inheriting a global default that later
	// turns out to have been wrong for one of them.
	Distros []string `json:"distros"`
	Arches  []string `json:"arches"`
	// MinDiskBytes is a floor on total disk, 0 for "no floor". Decimal bytes —
	// disks are sold in decimal GB and the operator is comparing against a
	// vendor's spec sheet, not against `df`.
	MinDiskBytes int64 `json:"minDiskBytes,omitempty"`
	// GPU is nil unless the type is useless without an accelerator.
	GPU *GPURequirement `json:"gpu,omitempty"`
}

// Requirement is one precondition rendered for a human, carrying the id the
// gate reports and the agent fact it is checked against. Fact is part of the
// contract on purpose: when a check cannot run because the agent never sent
// that datum, SIGMA-203 has to say which fact was missing instead of failing
// the host for a requirement nobody could evaluate.
type Requirement struct {
	ID   RequirementID `json:"id"`
	Fact string        `json:"fact"`
	Text string        `json:"text"`
}

// List renders the requirements in a fixed order (distro, arch, disk, GPU) so
// the gate's error and the dashboard's checklist read identically.
func (r ServerRequirements) List() []Requirement {
	out := make([]Requirement, 0, 4)
	if len(r.Distros) > 0 {
		out = append(out, Requirement{ID: ReqDistro, Fact: "distro",
			Text: "Runs " + joinOr(distroLabelsFor(r.Distros)) + "."})
	}
	if len(r.Arches) > 0 {
		out = append(out, Requirement{ID: ReqArch, Fact: "arch",
			Text: joinOr(r.Arches) + " CPU architecture."})
	}
	if r.MinDiskBytes > 0 {
		out = append(out, Requirement{ID: ReqDisk, Fact: "diskTotalBytes",
			Text: fmt.Sprintf("At least %s of disk.", formatDiskBytes(r.MinDiskBytes))})
	}
	if r.GPU != nil {
		vendor := strings.ToUpper(r.GPU.Vendor)
		text := "An " + vendor + " GPU."
		if r.GPU.Driver {
			text = "An " + vendor + " GPU with a usable driver."
		}
		out = append(out, Requirement{ID: ReqGPU, Fact: "gpu", Text: text})
	}
	return out
}

// ServerTypeSpec is everything the product knows about one server type.
type ServerTypeSpec struct {
	Type  string `json:"type"`
	Label string `json:"label"`
	// Hint is the one line the connect dialog shows under the type.
	Hint string `json:"hint"`
	// Hosts is this type's half of the availability matrix: the resource kinds
	// that may be scheduled onto it. The matrix hangs off the SERVER type, not
	// off the kind, precisely so that adding a type stays a one-entry edit;
	// AllowedServerTypes transposes it for the kind-first callers.
	Hosts []string `json:"hosts"`
	// HostsNothingReason explains an empty Hosts. An empty list with no reason
	// renders as an unexplained dead end in the UI, so buildCatalog panics on
	// one rather than letting it ship.
	HostsNothingReason string `json:"hostsNothingReason,omitempty"`
	// Connectable is whether the type is offered when connecting a NEW server.
	// It is UI guidance, NOT an authorization rule: the HTTP boundary accepts
	// every canonical type, because a boundary that accepts less than the store
	// is the bug this issue exists to remove.
	Connectable bool `json:"connectable"`
	// UnitWeight is the billing weight (server_units.go). Stated for every type
	// including the ordinary ones — `vps` and `build` used to be absent from
	// the weight table entirely and billed at the fallback by accident.
	UnitWeight int `json:"unitWeight"`
	// Requires is enforced at registration by SIGMA-203; nothing enforces it
	// today beyond the distro check the provisioner already had.
	Requires ServerRequirements `json:"requires"`
}

// Resource kinds, in the order the dashboard offers them, each naming the
// category it belongs to. Separate from the matrix on purpose: a kind that no
// server type can host is still a KNOWN kind (CreateResource must say "no
// server can host this" rather than "unknown kind"), and deriving the kind list
// from the matrix would lose that.
//
// The category is stated HERE, on the kind, and never as a list of members on
// the category: one kind cannot then land in two buckets or in none, which is
// the failure a hand-kept membership list has.
var resourceKinds = []struct {
	Kind     string
	Label    string
	Category string
}{
	{"app", "App", "application"},
	{"postgres", "PostgreSQL", "database"},
	{"mysql", "MySQL", "database"},
	{"mongodb", "MongoDB", "database"},
	{"redis", "Redis", "database"},
	{"s3", "Object storage", "storage"},
	{"llm", "LLM", "model"},
}

// The categories the New Resource wizard offers first, in the order it offers
// them — slice order IS display order, as it is for the kinds above and the
// server types below, so an ordering field cannot come to disagree with the
// list it orders.
//
// Categories exist because postgres, mysql, mongodb and redis are not peers of
// "Application": they are one decision made four times, and step 1 laid all
// seven kinds out flat as if they were equals. That grid also gets taller with
// every kind we add, which is the part that does not survive the next few.
//
// What a category must NOT become is a second click bought with the first. A
// category holding exactly one kind is a question with a single possible
// answer, and the dashboard resolves it without asking (see
// web/src/lib/wizard/steps.ts). That is why Kinds is not a field an editor
// fills in and why nothing here says "Application has one kind": the structure
// is the same for all four, and the day Application gains a second kind it
// starts showing a list without another line being written.
var resourceCategories = []struct {
	ID    string
	Label string
	Hint  string
}{
	{"application", "Application", "Build and deploy a repository."},
	{"database", "Database", "A managed engine with generated credentials."},
	{"model", "Model endpoint", "Serve a model from the Hub on GPU hardware."},
	{"storage", "Object storage", "S3-compatible buckets on a storage host."},
}

// The distros the hardened onboarding path accepts (SIGMA-A-5). Labelled here
// so the operator-facing sentence is generated instead of hand-written — it
// used to be typed out twice in api/registry.go, which meant adding a distro
// was a prose edit in two places that no test covered.
var supportedDistroLabels = []struct {
	ID    string
	Label string
}{
	{"ubuntu-22.04", "Ubuntu 22.04 LTS"},
	{"ubuntu-24.04", "Ubuntu 24.04 LTS"},
	{"debian-12", "Debian 12"},
}

// allDistros is every onboardable distro. Types narrow this when they have a
// reason to; none does today, and stating it per type is what makes narrowing
// one of them later a one-line edit instead of a new mechanism.
func allDistros() []string {
	out := make([]string, 0, len(supportedDistroLabels))
	for _, d := range supportedDistroLabels {
		out = append(out, d.ID)
	}
	return out
}

// Architectures. The agent itself ships amd64 and arm64 binaries
// (agent/internal/selfupdate), so those are the only two a host can run at all.
var (
	bothArches = []string{"amd64", "arm64"}
	amd64Only  = []string{"amd64"}
)

const (
	gb = 1_000_000_000

	// A managed engine is not just the dataset: it is the dataset plus WAL /
	// binlog plus a local basebackup taken before every restore. 100 GB is the
	// point below which routine maintenance is what fills the disk, and a
	// database that fills its disk stops accepting writes.
	databaseMinDisk = 100 * gb

	// Object storage exists to promise capacity. Below 500 GB the bucket is
	// something the operator could have kept next to the app, and calling the
	// host "storage" sets an expectation the hardware cannot meet.
	storageMinDisk = 500 * gb
)

// serverCatalog is the canonical list, in the order the dashboard presents it.
//
// The rules behind the matrix, so a future edit is a decision rather than a
// guess:
//   - "vps" is a general-purpose host that happens to be virtualized. It hosts
//     whatever a general server hosts; the difference is disclosure (shared
//     tenancy, burst CPU, no nested virt), not capability.
//   - "k8s" nodes host nothing directly. Their workloads arrive through the
//     cluster's control plane, so aiming a resource at a node individually is
//     always a mistake and is refused rather than quietly scheduled.
//   - "build" servers compile images and ship them to a registry; they run no
//     long-lived workloads of their own.
//   - "llm" needs a GPU. Serving a model on CPU is technically possible and
//     practically useless, so it is not offered.
var serverCatalog = []ServerTypeSpec{
	{
		Type:        "general",
		Label:       "General",
		Hint:        "Apps and databases on a machine you control end to end.",
		Hosts:       []string{"app", "postgres", "mysql", "mongodb", "redis"},
		Connectable: true,
		UnitWeight:  1,
		Requires:    ServerRequirements{Distros: allDistros(), Arches: bothArches},
	},
	{
		Type:        "vps",
		Label:       "VPS",
		Hint:        "A virtualized host — same capabilities, shared tenancy and burst CPU.",
		Hosts:       []string{"app", "postgres", "mysql", "mongodb", "redis"},
		Connectable: true,
		UnitWeight:  1,
		Requires:    ServerRequirements{Distros: allDistros(), Arches: bothArches},
	},
	{
		Type:        "database",
		Label:       "Database",
		Hint:        "Tuned for managed database engines with production-grade settings.",
		Hosts:       []string{"postgres", "mysql", "mongodb", "redis"},
		Connectable: true,
		UnitWeight:  1,
		Requires: ServerRequirements{
			Distros: allDistros(), Arches: bothArches, MinDiskBytes: databaseMinDisk,
		},
	},
	{
		Type:        "storage",
		Label:       "Storage",
		Hint:        "Large disks for S3-compatible object storage.",
		Hosts:       []string{"s3"},
		Connectable: true,
		UnitWeight:  1,
		Requires: ServerRequirements{
			Distros: allDistros(), Arches: bothArches, MinDiskBytes: storageMinDisk,
		},
	},
	{
		Type:        "gpu",
		Label:       "GPU",
		Hint:        "NVIDIA hardware for model hosting; drivers and the runtime are managed.",
		Hosts:       []string{"llm", "app"},
		Connectable: true,
		UnitWeight:  4,
		Requires: ServerRequirements{
			Distros: allDistros(),
			// amd64 only: the default inference runtime we pin
			// (vllm/vllm-openai, llm_engines.go) publishes x86_64 layers only,
			// so an arm64 GPU host would enroll happily and then fail to pull
			// its image on the first deploy.
			Arches: amd64Only,
			GPU:    &GPURequirement{Vendor: "nvidia", Driver: true},
		},
	},
	{
		Type:  "k8s",
		Label: "Cluster node",
		Hint:  "Joined to a cluster — workloads arrive through its control plane.",
		Hosts: nil,
		HostsNothingReason: "Cluster nodes receive workloads from the cluster's control plane, " +
			"not directly.",
		// Not offered at connect time: a node becomes a cluster member by
		// JOINING a cluster, never by being declared one at enrollment, so the
		// dialog would be creating a host nothing can be scheduled onto. The
		// API still accepts the type — see Connectable's doc comment.
		Connectable: false,
		UnitWeight:  2,
		Requires:    ServerRequirements{Distros: allDistros(), Arches: bothArches},
	},
	{
		Type:  "build",
		Label: "Build",
		Hint:  "Compiles images for other servers; runs no long-lived workloads.",
		Hosts: nil,
		HostsNothingReason: "Build servers compile images and push them to a registry; " +
			"they run no workloads.",
		Connectable: true,
		UnitWeight:  1,
		Requires:    ServerRequirements{Distros: allDistros(), Arches: bothArches},
	},
}

// ResourceCategorySpec is one bucket of the wizard's first screen: what it is
// called, the line under its card, and the kinds it holds in catalog order.
//
// Kinds is DERIVED from resourceKinds rather than typed out here — see the
// comment on resourceCategories for why membership is stated on the kind.
type ResourceCategorySpec struct {
	ID    string   `json:"id"`
	Label string   `json:"label"`
	Hint  string   `json:"hint"`
	Kinds []string `json:"kinds"`
}

// Derived indexes. Built once at init so every lookup is a map hit and, more
// importantly, so the consistency checks below run on package load: a catalog
// entry that names a kind nothing recognises is a programming error we want at
// startup, not at the first CreateResource of the week.
var (
	catalogByType     map[string]ServerTypeSpec
	catalogKinds      map[string]string   // kind → label
	kindServerTypes   map[string][]string // kind → server types (the transpose)
	catalogCategories map[string]bool     // category id → known
	categoryKinds     map[string][]string // category → kinds (the transpose)
	catalogDistros    map[string]bool
)

func init() {
	catalogByType = make(map[string]ServerTypeSpec, len(serverCatalog))
	catalogKinds = make(map[string]string, len(resourceKinds))
	kindServerTypes = make(map[string][]string, len(resourceKinds))
	catalogCategories = make(map[string]bool, len(resourceCategories))
	categoryKinds = make(map[string][]string, len(resourceCategories))
	catalogDistros = make(map[string]bool, len(supportedDistroLabels))

	for _, c := range resourceCategories {
		if catalogCategories[c.ID] {
			panic("store: duplicate resource category in catalog: " + c.ID)
		}
		if c.Label == "" || c.Hint == "" {
			panic("store: resource category has no label or hint: " + c.ID)
		}
		catalogCategories[c.ID] = true
		categoryKinds[c.ID] = []string{}
	}
	for _, k := range resourceKinds {
		catalogKinds[k.Kind] = k.Label
		if !catalogCategories[k.Category] {
			panic("store: resource kind " + k.Kind + " names unknown category " + k.Category)
		}
		categoryKinds[k.Category] = append(categoryKinds[k.Category], k.Kind)
		// Non-nil even when nothing hosts the kind: AllowedServerTypes uses nil
		// to mean "unknown kind", so a known-but-unhostable kind must not
		// masquerade as a typo.
		kindServerTypes[k.Kind] = []string{}
	}
	for _, c := range resourceCategories {
		// An empty category is a card that opens an empty list — the dead end
		// the wizard rework exists to delete, shipped by a one-line edit.
		if len(categoryKinds[c.ID]) == 0 {
			panic("store: resource category holds no kinds: " + c.ID)
		}
	}
	for _, d := range supportedDistroLabels {
		catalogDistros[d.ID] = true
	}
	for _, spec := range serverCatalog {
		if _, dup := catalogByType[spec.Type]; dup {
			panic("store: duplicate server type in catalog: " + spec.Type)
		}
		if len(spec.Hosts) == 0 && spec.HostsNothingReason == "" {
			panic("store: server type hosts nothing and says why not: " + spec.Type)
		}
		if spec.UnitWeight <= 0 {
			panic("store: server type has no billing weight: " + spec.Type)
		}
		if !serverTypePattern.MatchString(spec.Type) {
			// unitWeightSQL inlines these as SQL literals; the pattern is what
			// makes that safe by construction rather than by trust.
			panic("store: invalid server type in catalog: " + spec.Type)
		}
		for _, d := range spec.Requires.Distros {
			if !catalogDistros[d] {
				panic("store: server type " + spec.Type + " requires unknown distro " + d)
			}
		}
		for _, kind := range spec.Hosts {
			if _, ok := catalogKinds[kind]; !ok {
				panic("store: server type " + spec.Type + " hosts unknown kind " + kind)
			}
			kindServerTypes[kind] = append(kindServerTypes[kind], spec.Type)
		}
		catalogByType[spec.Type] = spec
	}
}

// ServerCatalog returns the canonical specs in presentation order. This is what
// the TypeScript generator renders and what the parity tests compare against.
func ServerCatalog() []ServerTypeSpec {
	out := make([]ServerTypeSpec, len(serverCatalog))
	copy(out, serverCatalog)
	return out
}

// ServerTypeSpecFor returns one type's spec.
func ServerTypeSpecFor(t string) (ServerTypeSpec, bool) {
	spec, ok := catalogByType[t]
	return spec, ok
}

// IsServerType reports whether a server type is known.
func IsServerType(t string) bool { _, ok := catalogByType[t]; return ok }

// ServerTypes lists the known server types in catalog order.
func ServerTypes() []string {
	out := make([]string, 0, len(serverCatalog))
	for _, spec := range serverCatalog {
		out = append(out, spec.Type)
	}
	return out
}

// AllowedServerTypes returns the server types a resource kind may run on, or
// nil for an unknown kind — callers distinguish the two, so a known kind that
// nothing can host returns an empty (non-nil) slice.
func AllowedServerTypes(kind string) []string {
	types, ok := kindServerTypes[kind]
	if !ok {
		return nil
	}
	out := make([]string, len(types))
	copy(out, types)
	return out
}

// ResourceKinds lists the known resource kinds in presentation order.
func ResourceKinds() []string {
	out := make([]string, 0, len(resourceKinds))
	for _, k := range resourceKinds {
		out = append(out, k.Kind)
	}
	return out
}

// ResourceCategories lists the category ids in the order the wizard offers
// them.
func ResourceCategories() []string {
	out := make([]string, 0, len(resourceCategories))
	for _, c := range resourceCategories {
		out = append(out, c.ID)
	}
	return out
}

// ResourceCategoryCatalog returns the categories in presentation order, each
// carrying the kinds inside it. This is what the TypeScript generator renders
// the step-1 picker from.
func ResourceCategoryCatalog() []ResourceCategorySpec {
	out := make([]ResourceCategorySpec, 0, len(resourceCategories))
	for _, c := range resourceCategories {
		kinds := make([]string, len(categoryKinds[c.ID]))
		copy(kinds, categoryKinds[c.ID])
		out = append(out, ResourceCategorySpec{ID: c.ID, Label: c.Label, Hint: c.Hint, Kinds: kinds})
	}
	return out
}

// ResourceKindLabel returns a kind's human label, or the raw kind if unknown.
func ResourceKindLabel(kind string) string {
	if label, ok := catalogKinds[kind]; ok {
		return label
	}
	return kind
}

// ServerRequirementsFor returns what a host must have to enroll as a type.
// SIGMA-203's registration gate is the intended caller.
func ServerRequirementsFor(t string) (ServerRequirements, bool) {
	spec, ok := catalogByType[t]
	if !ok {
		return ServerRequirements{}, false
	}
	return spec.Requires, true
}

// DistroSupported reports whether a normalized distro id is onboardable at all.
// The per-type answer is ServerRequirementsFor(t).Distros; this is the coarse
// check the provisioner runs before it knows anything else about the host.
func DistroSupported(distro string) bool { return catalogDistros[distro] }

// SupportedDistros lists the onboardable distro ids in catalog order.
func SupportedDistros() []string {
	out := make([]string, 0, len(supportedDistroLabels))
	for _, d := range supportedDistroLabels {
		out = append(out, d.ID)
	}
	return out
}

// SupportedDistroSentence renders the operator-facing list ("Ubuntu 22.04,
// Ubuntu 24.04 or Debian 12") so the rejection message follows the catalog
// instead of being retyped next to every check.
func SupportedDistroSentence() string {
	return joinOr(distroLabelsFor(SupportedDistros()))
}

// ConnectableServerTypes are the types the connect dialog offers.
func ConnectableServerTypes() []string {
	out := make([]string, 0, len(serverCatalog))
	for _, spec := range serverCatalog {
		if spec.Connectable {
			out = append(out, spec.Type)
		}
	}
	return out
}

// CanHost reports whether a server type may host a resource kind.
func CanHost(serverType, kind string) bool {
	spec, ok := catalogByType[serverType]
	if !ok {
		return false
	}
	for _, k := range spec.Hosts {
		if k == kind {
			return true
		}
	}
	return false
}

func distroLabelsFor(ids []string) []string {
	labels := make([]string, 0, len(ids))
	for _, id := range ids {
		label := id
		for _, d := range supportedDistroLabels {
			if d.ID == id {
				label = d.Label
				break
			}
		}
		labels = append(labels, label)
	}
	return labels
}

// joinOr renders a list as "a, b or c" — the form the messages read in.
func joinOr(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// formatDiskBytes renders a decimal-GB floor the way a hosting provider states
// it, so "at least 500 GB" can be compared against the plan being bought.
func formatDiskBytes(b int64) string {
	if b >= 1000*gb {
		return fmt.Sprintf("%g TB", float64(b)/float64(1000*gb))
	}
	return fmt.Sprintf("%d GB", b/gb)
}

// sortedTypes is a deterministic ordering helper for the places that need one
// (SQL generation, stable test output) without disturbing catalog order.
func sortedTypes() []string {
	out := ServerTypes()
	sort.Strings(out)
	return out
}
