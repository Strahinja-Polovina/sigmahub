package store

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/jackc/pgx/v5"

	"github.com/Strahinja-Polovina/sigmahub/cp/internal/kms"
)

const githubAppKeyName = "github_app_private_key"

// LoadGitHubAppKey returns the GitHub App's RSA private key from cp_secrets
// (held KMS-custody-wrapped, same boundary as the DSD signing key). Unlike
// the DSD key it cannot be generated here — GitHub mints it — so pemPath
// imports the downloaded PEM on boot: the key is wrapped into cp_secrets and
// the file can then be deleted from the host. A changed file re-imports
// (App key rotation); (nil, nil) means the App integration is simply not
// configured.
func (s *Store) LoadGitHubAppKey(ctx context.Context, custody kms.KeyCustody, pemPath string) (*rsa.PrivateKey, error) {
	var wrapped []byte
	err := s.Pool.QueryRow(ctx,
		`SELECT wrapped FROM cp_secrets WHERE name = $1`, githubAppKeyName).Scan(&wrapped)
	haveStored := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	var stored []byte // PKCS1 DER of the stored key, when present
	if haveStored {
		stored, err = custody.Unwrap(ctx, githubAppKeyName, wrapped)
		if err != nil {
			return nil, fmt.Errorf("unwrap github app key: %w", err)
		}
	}

	if pemPath != "" {
		pemBytes, err := os.ReadFile(pemPath)
		if err != nil {
			return nil, fmt.Errorf("read github app key %s: %w", pemPath, err)
		}
		key, err := parseRSAPrivateKeyPEM(pemBytes)
		if err != nil {
			return nil, fmt.Errorf("parse github app key %s: %w", pemPath, err)
		}
		der := x509.MarshalPKCS1PrivateKey(key)
		if !bytes.Equal(der, stored) {
			newWrapped, err := custody.Wrap(ctx, githubAppKeyName, der)
			if err != nil {
				return nil, fmt.Errorf("wrap github app key: %w", err)
			}
			if _, err := s.Pool.Exec(ctx, `
				INSERT INTO cp_secrets (name, wrapped) VALUES ($1, $2)
				ON CONFLICT (name) DO UPDATE SET wrapped = EXCLUDED.wrapped`,
				githubAppKeyName, newWrapped); err != nil {
				return nil, fmt.Errorf("store github app key: %w", err)
			}
		}
		return key, nil
	}

	if !haveStored {
		return nil, nil
	}
	key, err := x509.ParsePKCS1PrivateKey(stored)
	if err != nil {
		return nil, fmt.Errorf("stored github app key is corrupt: %w", err)
	}
	return key, nil
}

// parseRSAPrivateKeyPEM accepts the PKCS1 PEM GitHub serves for App keys, and
// PKCS8 for operators who converted it.
func parseRSAPrivateKeyPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block found")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("PKCS8 key is %T, want *rsa.PrivateKey", key)
		}
		return rsaKey, nil
	default:
		return nil, fmt.Errorf("unsupported PEM block %q", block.Type)
	}
}
