package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEnvPasswordOverridesLeftoverDefaultPersistedFile(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	if err := os.MkdirAll(filepath.Dir(persisted), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persisted, []byte("admin123\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_DATA_DIR", "")
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", "")
	t.Setenv("M365_ADMIN_PASSWORD", "Custom!Str0ng#2024")

	got, _, err := loadAdminPassword()
	if err != nil {
		t.Fatalf("loadAdminPassword error: %v", err)
	}
	if !checkPassword(got, "Custom!Str0ng#2024") {
		t.Fatalf("loadAdminPassword hash does not match env password")
	}
	b, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	var data adminPasswordData
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("persisted file not json: %v body=%q", err, string(b))
	}
	if !checkPassword(data.Hash, "Custom!Str0ng#2024") {
		t.Fatalf("env password not persisted as hash")
	}
}

func TestBootstrapPasswordUsesWritablePersistentPath(t *testing.T) {
	dir := t.TempDir()
	persisted := filepath.Join(dir, "data", "admin-password")
	bootstrap := filepath.Join(dir, "secret")
	if err := os.WriteFile(bootstrap, []byte("Bootstrap!Str0ng#2024\n"), 0400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("M365_ADMIN_PASSWORD_FILE", persisted)
	t.Setenv("M365_ADMIN_PASSWORD_BOOTSTRAP_FILE", bootstrap)
	t.Setenv("M365_ADMIN_PASSWORD", "")
	t.Setenv("M365_DATA_DIR", "")

	got, _, err := loadAdminPassword()
	if err != nil {
		t.Fatalf("loadAdminPassword error: %v", err)
	}
	if !checkPassword(got, "Bootstrap!Str0ng#2024") {
		t.Fatalf("loadAdminPassword hash mismatch")
	}
	newPw := "N3w!Bootstrap#2025X"
	if err := saveAdminPassword(newPw); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(persisted)
	if err != nil {
		t.Fatal(err)
	}
	var data adminPasswordData
	if err := json.Unmarshal(b, &data); err != nil {
		t.Fatalf("persisted not json: %v", err)
	}
	if !checkPassword(data.Hash, newPw) {
		t.Fatalf("persisted hash mismatch")
	}
}
