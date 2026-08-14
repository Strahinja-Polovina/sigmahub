package k8s

// Fetching the k3s installer. The URL is a compile-time constant: the control
// plane can ask for a node to be installed, never for an arbitrary URL to be
// downloaded and executed.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// installerURL is the official k3s install script.
const installerURL = "https://get.k3s.io"

// K3sVersion is the Kubernetes version every SigmaHub cluster node installs.
//
// Pinned rather than left to the install script's "stable" channel so a cluster's
// version is a property of the SigmaHub release that built it, not of the day its
// nodes happened to be provisioned — and so the version customers run is the one
// CI exercises against a real API server (.github/workflows/ci.yml K3S_VERSION).
// Moving this means moving that in the same change.
const K3sVersion = "v1.31.4+k3s1"

// maxInstallerBytes bounds the download (the script is ~50 KB).
const maxInstallerBytes = 2 << 20

func fetchInstaller(ctx context.Context) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, installerURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch k3s installer: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch k3s installer: %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInstallerBytes))
	if err != nil {
		return nil, fmt.Errorf("read k3s installer: %w", err)
	}
	return body, nil
}
