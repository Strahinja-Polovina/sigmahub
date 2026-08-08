package k8s

// Manifest rendering. Deliberately hand-rolled rather than pulling in the
// Kubernetes client libraries: the agent ships as one static binary and the
// workload shape here is small and fixed (Namespace + Secret + Deployment +
// Service + optional Ingress). Every value that reaches YAML goes through
// quoting, so a secret containing a colon or a newline cannot break out of its
// field and rewrite the document.

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// renderManifests builds the workload's YAML document.
func renderManifests(spec ApplySpec, ns string, secrets []Secret) (string, error) {
	replicas := spec.Replicas
	if replicas < 1 {
		replicas = 1
	}
	name := spec.Name
	var b strings.Builder

	// Namespace — created alongside the workload so a project's first cluster
	// deploy doesn't fail on a missing namespace.
	fmt.Fprintf(&b, "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", yamlStr(ns))

	// Env-var secrets ride a Secret object; file-mode ones are mounted from it.
	envSecrets := make([]Secret, 0, len(secrets))
	fileSecrets := make([]Secret, 0, len(secrets))
	for _, s := range secrets {
		if s.EnvVar {
			envSecrets = append(envSecrets, s)
		} else {
			fileSecrets = append(fileSecrets, s)
		}
	}
	secretName := name + "-secrets"
	if len(secrets) > 0 {
		b.WriteString("---\napiVersion: v1\nkind: Secret\ntype: Opaque\n")
		fmt.Fprintf(&b, "metadata:\n  name: %s\n  namespace: %s\ndata:\n", yamlStr(secretName), yamlStr(ns))
		sorted := append([]Secret(nil), secrets...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
		for _, s := range sorted {
			fmt.Fprintf(&b, "  %s: %s\n", yamlStr(s.Name), base64.StdEncoding.EncodeToString([]byte(s.Value)))
		}
	}

	// Deployment.
	b.WriteString("---\napiVersion: apps/v1\nkind: Deployment\n")
	fmt.Fprintf(&b, "metadata:\n  name: %s\n  namespace: %s\n  labels:\n    app: %s\n",
		yamlStr(name), yamlStr(ns), yamlStr(name))
	fmt.Fprintf(&b, "spec:\n  replicas: %d\n  selector:\n    matchLabels:\n      app: %s\n",
		replicas, yamlStr(name))
	fmt.Fprintf(&b, "  template:\n    metadata:\n      labels:\n        app: %s\n", yamlStr(name))
	b.WriteString("    spec:\n      containers:\n")
	fmt.Fprintf(&b, "        - name: %s\n          image: %s\n", yamlStr(name), yamlStr(spec.Image))

	if len(spec.Ports) > 0 {
		b.WriteString("          ports:\n")
		for _, p := range spec.Ports {
			if p <= 0 || p > 65535 {
				return "", fmt.Errorf("port %d is out of range", p)
			}
			fmt.Fprintf(&b, "            - containerPort: %d\n", p)
		}
	}

	if len(spec.Env) > 0 || len(envSecrets) > 0 {
		b.WriteString("          env:\n")
		keys := make([]string, 0, len(spec.Env))
		for k := range spec.Env {
			keys = append(keys, k)
		}
		sort.Strings(keys) // deterministic output: a resync must not churn the file
		for _, k := range keys {
			fmt.Fprintf(&b, "            - name: %s\n              value: %s\n",
				yamlStr(k), yamlStr(spec.Env[k]))
		}
		sort.Slice(envSecrets, func(i, j int) bool { return envSecrets[i].Name < envSecrets[j].Name })
		for _, s := range envSecrets {
			fmt.Fprintf(&b, "            - name: %s\n              valueFrom:\n                secretKeyRef:\n                  name: %s\n                  key: %s\n",
				yamlStr(s.Name), yamlStr(secretName), yamlStr(s.Name))
		}
	}

	// File-mode secrets mount read-only, matching the container path's contract
	// that a non-env secret never lands in the process environment.
	if len(fileSecrets) > 0 {
		b.WriteString("          volumeMounts:\n")
		fmt.Fprintf(&b, "            - name: secrets\n              mountPath: %s\n              readOnly: true\n",
			yamlStr(secretsMountDir))
		b.WriteString("      volumes:\n        - name: secrets\n          secret:\n")
		fmt.Fprintf(&b, "            secretName: %s\n            items:\n", yamlStr(secretName))
		sort.Slice(fileSecrets, func(i, j int) bool { return fileSecrets[i].Name < fileSecrets[j].Name })
		for _, s := range fileSecrets {
			fmt.Fprintf(&b, "              - key: %s\n                path: %s\n", yamlStr(s.Name), yamlStr(s.Name))
		}
	}

	// Service — only when the workload actually listens.
	if len(spec.Ports) > 0 {
		b.WriteString("---\napiVersion: v1\nkind: Service\n")
		fmt.Fprintf(&b, "metadata:\n  name: %s\n  namespace: %s\n", yamlStr(name), yamlStr(ns))
		fmt.Fprintf(&b, "spec:\n  selector:\n    app: %s\n  ports:\n", yamlStr(name))
		for _, p := range spec.Ports {
			fmt.Fprintf(&b, "    - port: %d\n      targetPort: %d\n", p, p)
		}
	}

	// Ingress for attached domains. Routed to the first port, which is the same
	// choice the server path makes when picking the load-balancer port.
	if len(spec.Hosts) > 0 && len(spec.Ports) > 0 {
		b.WriteString("---\napiVersion: networking.k8s.io/v1\nkind: Ingress\n")
		fmt.Fprintf(&b, "metadata:\n  name: %s\n  namespace: %s\n", yamlStr(name), yamlStr(ns))
		b.WriteString("spec:\n  rules:\n")
		for _, h := range spec.Hosts {
			fmt.Fprintf(&b, "    - host: %s\n      http:\n        paths:\n", yamlStr(h))
			b.WriteString("          - path: /\n            pathType: Prefix\n            backend:\n              service:\n")
			fmt.Fprintf(&b, "                name: %s\n                port:\n                  number: %d\n",
				yamlStr(name), spec.Ports[0])
		}
	}

	return b.String(), nil
}

// secretsMountDir mirrors the container path's tmpfs mount point, so an app
// reads its file-mode secrets from the same place on a server and in a cluster.
const secretsMountDir = "/run/secrets/sigmahub"

// yamlStr renders a value as a double-quoted YAML scalar with the escapes YAML
// requires. Quoting everything is what keeps a secret value (or a domain, or an
// env value) from being parsed as structure.
func yamlStr(v string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range v {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
