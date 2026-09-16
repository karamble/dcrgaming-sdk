package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func writeDurable(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err = tmp.Chmod(0600); err != nil {
		return err
	}
	if _, err = tmp.Write(data); err != nil {
		return err
	}
	if err = tmp.Sync(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}
func cloneRecord(in TableRecord) TableRecord {
	raw, _ := json.Marshal(in)
	var out TableRecord
	_ = json.Unmarshal(raw, &out)
	return out
}
