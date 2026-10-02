package runtime

import (
	"encoding/json"

	"github.com/karamble/dcrgaming-sdk/internal/fsync"
)

func syncDirectory(dir string) error { return fsync.Dir(dir) }
func cloneRecord(in TableRecord) TableRecord {
	raw, _ := json.Marshal(in)
	var out TableRecord
	_ = json.Unmarshal(raw, &out)
	return out
}
