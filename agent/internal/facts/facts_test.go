package facts

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testCollector is a collector with every host hook replaced. Nothing here may
// reach the real machine: the build host has no nvidia-smi, no interesting
// /etc/os-release and one disk, which is precisely why none of the three could
// be tested before they were made injectable.
func testCollector(t *testing.T) *Collector {
	t.Helper()
	return &Collector{
		dataRoot: "/var/lib/sigmad",
		// Short so the timeout path costs the suite milliseconds, not the real
		// five-second bound twice over.
		probeTimeout: 20 * time.Millisecond,
		diskUsage: func(context.Context, string) (uint64, uint64, error) {
			return 0, 0, errors.New("no disk hook set")
		},
		lookPath: func(string) (string, error) { return "", errors.New("not found") },
		run: func(context.Context, string, ...string) ([]byte, error) {
			t.Fatal("run called without a runner hook")
			return nil, nil
		},
	}
}

func TestParseOSRelease(t *testing.T) {
	const ubuntu = `PRETTY_NAME="Ubuntu 24.04.1 LTS"
NAME="Ubuntu"
VERSION_ID="24.04"
VERSION="24.04.1 LTS (Noble Numbat)"
VERSION_CODENAME=noble
ID=ubuntu
ID_LIKE=debian
HOME_URL="https://www.ubuntu.com/"
`
	const debian = `PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"
NAME="Debian GNU/Linux"
VERSION_ID="12"
VERSION="12 (bookworm)"
ID=debian
`
	for _, tc := range []struct {
		name       string
		body       string
		wantID     string
		wantPretty string
	}{
		{"ubuntu 24.04", ubuntu, "ubuntu-24.04", "Ubuntu 24.04.1 LTS"},
		{"debian 12", debian, "debian-12", "Debian GNU/Linux 12 (bookworm)"},
		// The catalog ids the agent must reproduce, spelled out so a change to
		// either vocabulary breaks here rather than at a customer's enrollment.
		{"ubuntu 22.04", "ID=ubuntu\nVERSION_ID=\"22.04\"\n", "ubuntu-22.04", ""},
		{"single quoted", "ID='ubuntu'\nVERSION_ID='24.04'\n", "ubuntu-24.04", ""},
		{"unquoted", "ID=debian\nVERSION_ID=12\n", "debian-12", ""},
		{"uppercase and spaces", " ID = \"Ubuntu\" \nVERSION_ID=\"24.04\"\n", "ubuntu-24.04", ""},
		{"comments and blank lines", "# a comment\n\nID=debian\n\nVERSION_ID=12\n", "debian-12", ""},
		// A rolling release genuinely has no version. "arch" is a truthful id
		// the registration gate can refuse by name; "" would mean "could not
		// ask" and leave the gate with nothing to tell the operator.
		{"no version id", "ID=arch\nPRETTY_NAME=\"Arch Linux\"\n", "arch", "Arch Linux"},
		{"space in id", "ID=\"opensuse leap\"\nVERSION_ID=15.6\n", "opensuse-leap-15.6", ""},
		{"escaped quote in pretty name", `PRETTY_NAME="Weird \"distro\""` + "\nID=weird\n", "weird", `Weird "distro"`},

		// Degradation cases: every one of these must come back empty instead of
		// panicking or inventing an id.
		{"empty file", "", "", ""},
		{"only whitespace", "   \n\n\t\n", "", ""},
		{"no key/value lines at all", "this is not an os-release file\n", "", ""},
		{"binary garbage", "\x00\x01\x02\xff\xfe", "", ""},
		{"version without id", "VERSION_ID=\"24.04\"\n", "", ""},
		{"id with no usable characters", "ID=\"!!!\"\nVERSION_ID=12\n", "", ""},
		// Truncated file: no ID survives, so the machine-checkable answer is
		// empty. The display string keeps its dangling quote rather than being
		// half-stripped — visibly wrong beats silently truncated.
		{"truncated mid-line", "PRETTY_NAME=\"Ubuntu 24.0", "", `"Ubuntu 24.0`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, pretty := ParseOSRelease(tc.body)
			if id != tc.wantID {
				t.Errorf("id = %q, want %q", id, tc.wantID)
			}
			if pretty != tc.wantPretty {
				t.Errorf("pretty = %q, want %q", pretty, tc.wantPretty)
			}
		})
	}
}

func TestCollectDistroFileHandling(t *testing.T) {
	dir := t.TempDir()
	etc := filepath.Join(dir, "etc-os-release")
	usrLib := filepath.Join(dir, "usrlib-os-release")

	t.Run("missing everywhere", func(t *testing.T) {
		c := testCollector(t)
		c.osReleasePaths = []string{etc, usrLib}
		if id, pretty := c.collectDistro(); id != "" || pretty != "" {
			t.Fatalf("missing os-release → (%q, %q), want empty", id, pretty)
		}
	})

	t.Run("falls back to /usr/lib", func(t *testing.T) {
		if err := os.WriteFile(usrLib, []byte("ID=debian\nVERSION_ID=12\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := testCollector(t)
		c.osReleasePaths = []string{etc, usrLib}
		if id, _ := c.collectDistro(); id != "debian-12" {
			t.Fatalf("id = %q, want debian-12", id)
		}
	})

	t.Run("prefers /etc", func(t *testing.T) {
		if err := os.WriteFile(etc, []byte("ID=ubuntu\nVERSION_ID=24.04\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := testCollector(t)
		c.osReleasePaths = []string{etc, usrLib}
		if id, _ := c.collectDistro(); id != "ubuntu-24.04" {
			t.Fatalf("id = %q, want ubuntu-24.04", id)
		}
	})

	t.Run("unreadable file degrades to empty", func(t *testing.T) {
		c := testCollector(t)
		c.osReleasePaths = []string{filepath.Join(dir, "does-not-exist"), dir /* a directory, not a file */}
		if id, _ := c.collectDistro(); id != "" {
			t.Fatalf("id = %q, want empty", id)
		}
	})
}

const (
	mib = 1024 * 1024
	gb  = 1_000_000_000
)

func TestParseNvidiaSMI(t *testing.T) {
	t.Run("one card", func(t *testing.T) {
		inv := parseNvidiaSMI("0, NVIDIA A10G, 23028, 535.183.01\n")
		if inv.Vendor != VendorNVIDIA || inv.Count != 1 {
			t.Fatalf("vendor/count = %q/%d, want nvidia/1", inv.Vendor, inv.Count)
		}
		if inv.Model != "NVIDIA A10G" {
			t.Errorf("model = %q", inv.Model)
		}
		if inv.DriverVersion != "535.183.01" {
			t.Errorf("driver = %q", inv.DriverVersion)
		}
		// The whole point of the schema: bytes, and specifically the ENUMERATED
		// bytes. A card sold as "24GB" has 23028 MiB ≈ 24.1e9 usable — close
		// enough to mislead and far enough to OOM a 24 GB model.
		want := uint64(23028 * mib)
		if inv.VRAMBytesPerGPU != want || inv.VRAMBytesTotal != want {
			t.Fatalf("vram per/total = %d/%d, want %d/%d",
				inv.VRAMBytesPerGPU, inv.VRAMBytesTotal, want, want)
		}
		if len(inv.Cards) != 1 || inv.Cards[0].Index != 0 || inv.Cards[0].VRAMBytes != want {
			t.Fatalf("cards = %+v", inv.Cards)
		}
	})

	t.Run("two cards", func(t *testing.T) {
		inv := parseNvidiaSMI(
			"0, NVIDIA A100-SXM4-40GB, 40960, 550.54.15\n" +
				"1, NVIDIA A100-SXM4-40GB, 40960, 550.54.15\n")
		if inv.Count != 2 {
			t.Fatalf("count = %d, want 2", inv.Count)
		}
		if inv.Model != "NVIDIA A100-SXM4-40GB" {
			t.Errorf("model = %q", inv.Model)
		}
		if inv.VRAMBytesPerGPU != 40960*mib {
			t.Errorf("per-gpu = %d, want %d", inv.VRAMBytesPerGPU, uint64(40960*mib))
		}
		if inv.VRAMBytesTotal != 2*40960*mib {
			t.Errorf("total = %d, want %d", inv.VRAMBytesTotal, uint64(2*40960*mib))
		}
		if len(inv.Cards) != 2 || inv.Cards[1].Index != 1 {
			t.Fatalf("cards = %+v", inv.Cards)
		}
	})

	t.Run("mixed cards report the smallest per-gpu figure", func(t *testing.T) {
		inv := parseNvidiaSMI(
			"0, NVIDIA A100-SXM4-40GB, 40960, 550.54.15\n" +
				"1, NVIDIA GeForce RTX 4090, 24564, 550.54.15\n")
		if inv.VRAMBytesPerGPU != 24564*mib {
			t.Fatalf("per-gpu = %d, want the smaller card's %d",
				inv.VRAMBytesPerGPU, uint64(24564*mib))
		}
		// No single model is true of this host, so the summary field stays
		// empty and Cards is the answer.
		if inv.Model != "" {
			t.Errorf("model = %q, want empty for a mixed host", inv.Model)
		}
		if len(inv.Cards) != 2 {
			t.Fatalf("cards = %+v", inv.Cards)
		}
	})

	t.Run("tolerates a unit suffix", func(t *testing.T) {
		inv := parseNvidiaSMI("0, NVIDIA L4, 23034 MiB, 535.161.07\n")
		if inv.VRAMBytesPerGPU != 23034*mib {
			t.Fatalf("per-gpu = %d, want %d", inv.VRAMBytesPerGPU, uint64(23034*mib))
		}
	})

	t.Run("model name containing a comma", func(t *testing.T) {
		inv := parseNvidiaSMI("0, Fancy GPU, Rev B, 16384, 555.42.02\n")
		if inv.Model != "Fancy GPU, Rev B" {
			t.Errorf("model = %q", inv.Model)
		}
		if inv.VRAMBytesPerGPU != 16384*mib || inv.DriverVersion != "555.42.02" {
			t.Fatalf("vram/driver = %d/%q", inv.VRAMBytesPerGPU, inv.DriverVersion)
		}
	})

	t.Run("unreadable output is no GPUs", func(t *testing.T) {
		for _, out := range []string{
			"",
			"\n\n",
			"No devices were found\n",
			"0, NVIDIA A10G\n",             // truncated row
			"x, NVIDIA A10G, 23028, 535\n", // non-numeric index
			"0, NVIDIA A10G, [N/A], 535\n", // driver present, memory unreadable
		} {
			if inv := parseNvidiaSMI(out); inv.Count != 0 || inv.Vendor != "" {
				t.Fatalf("parseNvidiaSMI(%q) = %+v, want an empty inventory", out, inv)
			}
		}
	})

	t.Run("skips one bad row among good ones", func(t *testing.T) {
		inv := parseNvidiaSMI(
			"0, NVIDIA A10G, 23028, 535.183.01\n" +
				"garbage\n" +
				"1, NVIDIA A10G, 23028, 535.183.01\n")
		if inv.Count != 2 {
			t.Fatalf("count = %d, want 2", inv.Count)
		}
	})
}

func TestCollectGPU(t *testing.T) {
	ctx := context.Background()

	t.Run("tool absent means no GPUs, not an error", func(t *testing.T) {
		c := testCollector(t)
		// run stays the t.Fatal hook: an absent tool must not be executed.
		inv, _ := c.collectGPU(ctx)
		if inv.Count != 0 || inv.Vendor != "" || inv.DriverVersion != "" {
			t.Fatalf("inventory = %+v, want empty", inv)
		}
	})

	t.Run("tool present with one card", func(t *testing.T) {
		c := testCollector(t)
		var gotArgs []string
		c.lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
		c.run = func(_ context.Context, name string, args ...string) ([]byte, error) {
			if name != "nvidia-smi" {
				t.Fatalf("ran %q", name)
			}
			gotArgs = args
			return []byte("0, NVIDIA A10G, 23028, 535.183.01\n"), nil
		}
		inv, _ := c.collectGPU(ctx)
		if inv.Count != 1 || inv.VRAMBytesPerGPU != 23028*mib {
			t.Fatalf("inventory = %+v", inv)
		}
		if len(gotArgs) != 2 || gotArgs[1] != "--format=csv,noheader,nounits" {
			t.Fatalf("args = %q", gotArgs)
		}
	})

	t.Run("tool present with two cards", func(t *testing.T) {
		c := testCollector(t)
		c.lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
		c.run = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("0, NVIDIA L40S, 46068, 550.54.15\n1, NVIDIA L40S, 46068, 550.54.15\n"), nil
		}
		inv, _ := c.collectGPU(ctx)
		if inv.Count != 2 || inv.VRAMBytesTotal != 2*46068*mib {
			t.Fatalf("inventory = %+v", inv)
		}
	})

	t.Run("tool present but the driver refuses", func(t *testing.T) {
		c := testCollector(t)
		c.lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
		c.run = func(context.Context, string, ...string) ([]byte, error) {
			return []byte("NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver\n"),
				errors.New("exit status 9")
		}
		// A card nothing can talk to cannot serve a model; reporting it as
		// present is how a host gets enrolled at GPU weight and then fails its
		// first deploy.
		if inv, _ := c.collectGPU(ctx); inv.Count != 0 || inv.Vendor != "" {
			t.Fatalf("inventory = %+v, want empty", inv)
		}
	})
}

func TestCollectDisk(t *testing.T) {
	ctx := context.Background()

	t.Run("measures the data root", func(t *testing.T) {
		c := testCollector(t)
		c.diskUsage = func(_ context.Context, path string) (uint64, uint64, error) {
			if path != "/var/lib/sigmad" {
				t.Fatalf("measured %q", path)
			}
			return 512 * gb, 300 * gb, nil
		}
		path, total, free := c.collectDisk(ctx)
		if path != "/var/lib/sigmad" || total != 512*gb || free == nil || *free != 300*gb {
			t.Fatalf("collectDisk = %q/%d/%v", path, total, free)
		}
	})

	t.Run("falls back to the filesystem root", func(t *testing.T) {
		c := testCollector(t)
		c.diskUsage = func(_ context.Context, path string) (uint64, uint64, error) {
			if path == "/" {
				return 100 * gb, 40 * gb, nil
			}
			return 0, 0, errors.New("no such file or directory")
		}
		path, total, _ := c.collectDisk(ctx)
		if path != "/" || total != 100*gb {
			t.Fatalf("collectDisk = %q/%d, want / and 100GB", path, total)
		}
	})

	t.Run("a zero total is treated as unreadable", func(t *testing.T) {
		// A pseudo-filesystem answers with Total 0. Reporting 0 bytes would
		// read as "this host has no disk" and fail every disk floor.
		c := testCollector(t)
		c.diskUsage = func(context.Context, string) (uint64, uint64, error) { return 0, 0, nil }
		path, total, free := c.collectDisk(ctx)
		if path != "" || total != 0 || free != nil {
			t.Fatalf("collectDisk = %q/%d/%v, want all zero and no free reading", path, total, free)
		}
	})
}

// TestCollectJSONShape pins the wire schema. The control plane, the
// registration gate (SIGMA-203) and the VRAM fit estimate (SIGMA-214) all key
// off these exact names; renaming one is a silent behaviour change everywhere,
// not a compile error, because the payload crosses the module boundary as JSON.
func TestCollectJSONShape(t *testing.T) {
	dir := t.TempDir()
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := testCollector(t)
	c.osReleasePaths = []string{osRelease}
	c.diskUsage = func(context.Context, string) (uint64, uint64, error) {
		return 1024 * gb, 512 * gb, nil
	}
	c.lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
	c.run = func(context.Context, string, ...string) ([]byte, error) {
		return []byte("0, NVIDIA L40S, 46068, 550.54.15\n1, NVIDIA L40S, 46068, 550.54.15\n"), nil
	}

	raw, err := json.Marshal(c.Collect(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]any{
		"distro":         "ubuntu-24.04",
		"distroName":     "Ubuntu 24.04.1 LTS",
		"diskTotalBytes": float64(1024 * gb),
		"diskFreeBytes":  float64(512 * gb),
		"diskPath":       "/var/lib/sigmad",
	} {
		if got[key] != want {
			t.Errorf("facts[%q] = %v, want %v", key, got[key], want)
		}
	}
	gpu, ok := got["gpu"].(map[string]any)
	if !ok {
		t.Fatalf("facts.gpu = %v, want an object", got["gpu"])
	}
	for key, want := range map[string]any{
		"vendor":          "nvidia",
		"model":           "NVIDIA L40S",
		"count":           float64(2),
		"vramBytesPerGpu": float64(46068 * mib),
		"vramBytesTotal":  float64(2 * 46068 * mib),
		"driverVersion":   "550.54.15",
	} {
		if gpu[key] != want {
			t.Errorf("facts.gpu[%q] = %v, want %v", key, gpu[key], want)
		}
	}
	cards, ok := gpu["cards"].([]any)
	if !ok || len(cards) != 2 {
		t.Fatalf("facts.gpu.cards = %v, want 2 entries", gpu["cards"])
	}
	card0, _ := cards[0].(map[string]any)
	if card0["index"] != float64(0) || card0["vramBytes"] != float64(46068*mib) {
		t.Errorf("facts.gpu.cards[0] = %v", cards[0])
	}
}

// A host with nothing to report must still produce a payload the control plane
// reads as "known, and there is none" for GPU and "unknown" for everything
// else. The distinction is load-bearing: absent keys are preserved by the CP,
// and an absent gpu key would mean a machine that lost its card kept it forever.
func TestCollectDegradedHostOmitsUnknownFacts(t *testing.T) {
	c := testCollector(t)
	c.osReleasePaths = []string{filepath.Join(t.TempDir(), "absent")}
	c.diskUsage = func(context.Context, string) (uint64, uint64, error) {
		return 0, 0, errors.New("nope")
	}

	raw, err := json.Marshal(c.Collect(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"distro", "distroName", "diskTotalBytes", "diskFreeBytes", "diskPath"} {
		if _, present := got[key]; present {
			t.Errorf("facts[%q] present as %v; an unknown fact must be OMITTED so the CP keeps the last known value", key, got[key])
		}
	}
	gpu, ok := got["gpu"].(map[string]any)
	if !ok {
		t.Fatalf("facts.gpu = %v, want an object even with no GPU", got["gpu"])
	}
	if gpu["vendor"] != "" || gpu["count"] != float64(0) {
		t.Fatalf("facts.gpu = %v, want an explicit empty inventory", gpu)
	}
}

// A full disk is the reading that matters most, and it is reachable: gopsutil
// reports the unprivileged-available figure, which hits zero on ext4 while the
// root reserve still has room. It used to be indistinguishable from "could not
// stat the mount" — both arrived as a plain 0 that the control plane discarded
// as a failed probe — so a full disk kept advertising its last healthy figure.
func TestCollectDiskReportsAGenuineZeroFree(t *testing.T) {
	ctx := context.Background()
	c := testCollector(t)
	c.diskUsage = func(context.Context, string) (uint64, uint64, error) { return 512 * gb, 0, nil }

	_, total, free := c.collectDisk(ctx)
	if total != 512*gb {
		t.Fatalf("total = %d", total)
	}
	if free == nil {
		t.Fatal("a full disk must REPORT zero free, not omit the reading")
	}
	if *free != 0 {
		t.Fatalf("free = %d, want 0", *free)
	}

	// And it survives serialization: omitempty on a plain uint64 would drop it.
	f := Facts{DiskTotalBytes: 512 * gb, DiskFreeBytes: free}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"diskFreeBytes":0`) {
		t.Fatalf("marshalled facts = %s, want an explicit zero free", b)
	}
}

// A five-second deadline on a busy card is not evidence the card is gone. The
// control plane MERGES facts, so reporting an empty inventory here wiped a real
// multi-GPU box on one slow heartbeat — and both the enrollment gate and the
// LLM fit check read that field.
func TestCollectGPUTimeoutReportsNothingRatherThanNone(t *testing.T) {
	c := testCollector(t)
	c.lookPath = func(string) (string, error) { return "/usr/bin/nvidia-smi", nil }
	c.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if _, ok := c.collectGPU(context.Background()); ok {
		t.Fatal("a timed-out probe must be inconclusive, or it erases a real inventory")
	}

	// The whole Facts payload then omits the key, which is what makes the
	// control plane's merge leave the stored inventory standing.
	f := c.Collect(context.Background())
	if f.GPU != nil {
		t.Fatalf("facts.gpu = %+v, want it omitted after an inconclusive probe", f.GPU)
	}

	// A driver that answers "no" is a real reading and must still be reported:
	// a pulled card has to be forgettable.
	c.run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("NVIDIA-SMI has failed because it couldn't communicate with the driver")
	}
	inv, ok := c.collectGPU(context.Background())
	if !ok {
		t.Fatal("a driver refusing to answer is conclusive: it cannot run a model either")
	}
	if inv.Count != 0 {
		t.Fatalf("inventory = %+v, want empty", inv)
	}
}

// Facts merge, so a key that is merely omitted keeps its previous value. Docker
// being removed from a host has to CLEAR the version, not leave the dashboard
// showing the version of a daemon that is no longer installed.
func TestDockerVersionIsAlwaysSent(t *testing.T) {
	b, err := json.Marshal(Facts{DockerAvailable: false})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"dockerVersion":""`) {
		t.Fatalf("marshalled facts = %s, want an explicit empty dockerVersion", b)
	}
}
