package config

import (
	"encoding/json"
	"os"
	"path/filepath"
)

type Account struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName,omitempty"`
	Status      string `json:"status"`
}

type Store struct {
	Accounts []Account `json:"accounts"`
}

func Path() string {
	if p := os.Getenv("M365_CONFIG"); p != "" {
		return p
	}
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".config", "m365-copilot2api", "accounts.json")
}

func Load() (Store, error) {
	b, e := os.ReadFile(Path())
	if os.IsNotExist(e) {
		return Store{Accounts: []Account{}}, nil
	}
	if e != nil {
		return Store{}, e
	}
	var s Store
	e = json.Unmarshal(b, &s)
	return s, e
}

func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func writeFileAtomic(path string, b []byte, perm os.FileMode) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		tmp.Close()
		os.Remove(tmpName)
	}()
	if err := tmp.Chmod(perm); err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	_ = fsyncDir(dir)
	return nil
}

func Save(s Store) error {
	p := Path()
	if e := os.MkdirAll(filepath.Dir(p), 0o700); e != nil {
		return e
	}
	b, e := json.MarshalIndent(s, "", "  ")
	if e != nil {
		return e
	}
	return writeFileAtomic(p, b, 0o600)
}
