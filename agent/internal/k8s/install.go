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
