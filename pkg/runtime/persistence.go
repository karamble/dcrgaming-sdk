package runtime

import (
	"encoding/json"
	"os"
)

func syncDirectory(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func cloneRecord(in TableRecord) TableRecord {
	raw, _ := json.Marshal(in)
	var out TableRecord
	_ = json.Unmarshal(raw, &out)
	return out
}
