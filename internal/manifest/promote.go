package manifest

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PromoteIfTrial atomically changes a trial tool's manifest status to active.
func PromoteIfTrial(t *Tool) (bool, error) {
	current, err := Load(t.Dir)
	if err != nil {
		return false, err
	}
	if current.Status != "trial" {
		return false, nil
	}
	current.Status = "active"
	raw, err := json.MarshalIndent(current, "", "  ")
	if err != nil {
		return false, err
	}
	raw = append(raw, '\n')
	if err := Validate(raw); err != nil {
		return false, err
	}

	path := filepath.Join(current.Dir, FileName)
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	f, err := os.CreateTemp(current.Dir, ".tool.json-*")
	if err != nil {
		return false, err
	}
	temp := f.Name()
	defer os.Remove(temp)
	if err := f.Chmod(info.Mode().Perm()); err != nil {
		f.Close()
		return false, err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	if err := os.Rename(temp, path); err != nil {
		return false, fmt.Errorf("replace %s: %w", path, err)
	}
	t.Status = "active"
	return true, nil
}
