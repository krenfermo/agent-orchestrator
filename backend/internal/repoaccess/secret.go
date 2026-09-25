package repoaccess

import (
	"path"
	"strings"
)

// secret.go — the shared secret boundary.
//
// This replaces two lists that had drifted apart (codegraph.DeniedPath and
// projectmemory's excludedFromSignals) and adds what neither had: cloud and
// tool credential stores, and credential DIRECTORIES as well as file names.
//
// The rule is refusal BEFORE opening. A file matching here is never read, so a
// later bug cannot leak bytes that never entered the process. Knowing that a
// configuration TEMPLATE exists is still useful to an agent, so
// IsSecretTemplate lets a caller surface "a configuration template exists"
// as metadata -- its content is still never read.
//
// Deliberately NOT denied: source files whose NAME mentions secrets
// (`secrets.go`, `credentials.ts`, `token_store.py`). They are code that
// HANDLES secrets, and blinding the indexers to them would hide exactly the
// code a reviewer most needs. The data files that hold values are named
// explicitly below.

// secretBaseNames are files whose content is a secret by convention.
var secretBaseNames = map[string]bool{
	// env and shell credential files
	".env": true, ".envrc": true, ".netrc": true, ".npmrc": true, ".pypirc": true,
	".pgpass": true, ".my.cnf": true, ".git-credentials": true, ".htpasswd": true,
	".dockercfg": true, ".vault-token": true,
	// generic credential data files
	"credentials": true, "credentials.json": true, "credentials.yaml": true, "credentials.yml": true,
	"secrets.json": true, "secrets.yaml": true, "secrets.yml": true, "secrets.toml": true,
	"secrets.env": true, "secret.json": true, "tokens.json": true, "token.json": true,
	// SSH private keys (public keys and known_hosts are not secret and stay
	// readable).
	"id_rsa": true, "id_dsa": true, "id_ecdsa": true, "id_ed25519": true,
	"authorized_keys": true,
	// cloud and cluster credentials
	"kubeconfig": true, "application_default_credentials.json": true,
	"service-account.json": true, "service_account.json": true, "serviceaccount.json": true,
	"terraform.tfstate": true, "terraform.tfstate.backup": true,
	// AO's own agent credential material, should a copy ever land in a repo
	"agent-credential.json": true,
}

// secretExtensions only ever hold key material or secret state.
var secretExtensions = map[string]bool{
	".pem": true, ".key": true, ".p12": true, ".pfx": true, ".jks": true,
	".keystore": true, ".crt": true, ".cer": true, ".der": true, ".asc": true,
	".gpg": true, ".kdbx": true, ".ppk": true, ".tfstate": true, ".tfvars": true,
	".age": true,
}

// secretDirNames are directories whose contents are credential stores. The
// basename rules catch `config/credentials.json`; they cannot catch
// `secrets/prod.yaml`, whose name says nothing and whose directory says
// everything.
var secretDirNames = map[string]bool{
	"secrets": true, ".secrets": true, "credentials": true, ".credentials": true,
	".ssh": true, ".gnupg": true, ".aws": true, ".azure": true, ".gcloud": true,
	".kube": true, ".docker": true, "agent-credentials": true,
}

// envTemplateSuffixes mark an env file that documents keys without values.
var envTemplateSuffixes = []string{".example", ".sample", ".template", ".dist", ".defaults"}

// IsSecretPath reports whether a repository-relative path must never be opened
// by an indexer. It is decided from the path alone.
func IsSecretPath(rel string) bool {
	norm := strings.ToLower(strings.Trim(strings.ReplaceAll(rel, "\\", "/"), "/"))
	if norm == "" {
		return false
	}
	base := path.Base(norm)
	if secretBaseNames[base] {
		return true
	}
	if secretExtensions[path.Ext(base)] {
		return true
	}
	// Every .env variant, templates included: a template is documentation by
	// intent, and a real value pasted into one is exactly the accident this
	// boundary exists for. Its existence can still be reported
	// (IsSecretTemplate); its content is not read.
	if base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env") {
		return true
	}
	dirs := strings.Split(norm, "/")
	for _, seg := range dirs[:len(dirs)-1] {
		if secretDirNames[seg] {
			return true
		}
	}
	return false
}

// IsSecretTemplate reports whether a secret-denied path is an env TEMPLATE
// (`.env.example`, `.env.sample`, …). Such a path is still never opened; a
// caller may record that it exists, as "configuration template exists", so an
// agent can be pointed at it without AO having read it.
func IsSecretTemplate(rel string) bool {
	base := strings.ToLower(path.Base(strings.ReplaceAll(rel, "\\", "/")))
	if !strings.HasPrefix(base, ".env.") {
		return false
	}
	for _, suffix := range envTemplateSuffixes {
		if strings.HasSuffix(base, suffix) {
			return true
		}
	}
	return false
}
